package join

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
)

type Runner struct {
	Identity inventory.STS
	Cloud    provision.EC2
	Store    bootstrap.Store
	Remote   node.Backend
	Owner    string
	Progress func(string)
	Wait     func(context.Context) error
}

func (r Runner) scope(ctx context.Context, p Plan, record provision.Record) error {
	if record.Bootstrap == nil || record.Bootstrap.PlanID != p.Bootstrap.ID || record.Bootstrap.Phase != "hosts-ready" || record.Deployment != nil || record.Upgrade != nil {
		return errors.New("complete exact fresh bootstrap; genesis deployments cannot be joined")
	}
	live, e := provision.RunningTargets(ctx, p.Bootstrap.Compute, record, r.Cloud)
	if e != nil {
		return e
	}
	for _, t := range p.Targets {
		v := live[t.Name]
		ip := aws.ToString(v.PrivateIpAddress)
		if p.Bootstrap.Access.Address == "public" {
			ip = aws.ToString(v.PublicIpAddress)
		}
		if aws.ToString(v.InstanceId) != t.InstanceID || ip != t.SSHAddress || aws.ToString(v.PrivateIpAddress) != t.PeerAddress {
			return fmt.Errorf("join host identity/placement drift: %s", t.Name)
		}
	}
	return nil
}
func ready(p Plan, o node.Observation) bool {
	c := o.Core
	return c != nil && c.Genesis == p.Chain.Genesis && c.CheckpointHash == p.Chain.CheckpointHash && c.Height >= p.Chain.CheckpointHeight && c.Headers <= c.Height && c.Synced && !c.IBD && c.Peers > 0 && c.Restarts == 0 && c.ChainLockHeight > 0 && c.ChainLockHeight <= c.Height && c.Height-c.ChainLockHeight <= 6
}
func (r Runner) Execute(ctx context.Context, p Plan) (result provision.Record, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	if r.Owner == "" || r.Remote == nil {
		return result, errors.New("runner identity and authenticated backend required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, errors.New("join requires deadline")
	}
	if err = provision.VerifyAccount(ctx, p.Bootstrap.Compute.Network.AWS.AccountID, r.Identity); err != nil {
		return
	}
	prior, _, err := r.Store.Read(ctx, p.Bootstrap.Compute)
	if err != nil {
		return result, err
	}
	if err = r.scope(ctx, p, prior); err != nil {
		return
	}
	rec, err := r.Store.Acquire(ctx, p.Bootstrap.Compute, r.Owner)
	if err != nil {
		return result, err
	}
	changed := false
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err != nil && changed {
			rec.Join.Phase = "interrupted"
			rec.LastError = node.SafeError(err)
			rec.UpdatedAt = time.Now().UTC()
			rec.Revision++
			err = errors.Join(err, r.Store.Save(c, rec, r.Owner))
		}
		err = errors.Join(err, r.Store.Release(c, p.Bootstrap.Compute, r.Owner))
		result = rec
	}()
	if err = r.scope(ctx, p, rec); err != nil {
		return
	}
	if rec.Join != nil && rec.Join.PlanID != p.ID {
		return result, errors.New("different join intent already owns allocation")
	}
	if rec.Join == nil {
		rec.Join = &provision.JoinProgress{PlanID: p.ID, Nodes: map[string]provision.JoinNode{}}
	}
	rec.Join.Phase = "joining"
	rec.LastRunner = r.Owner
	rec.LastError = ""
	changed = true
	for _, t := range p.Targets {
		rec.Join.Nodes[t.Name] = provision.JoinNode{Phase: "pending"}
	}
	save := func() error { rec.UpdatedAt = time.Now().UTC(); rec.Revision++; return r.Store.Save(ctx, rec, r.Owner) }
	if err = save(); err != nil {
		return
	}
	for _, t := range p.Targets {
		if r.Progress != nil {
			r.Progress("joining " + t.Name)
		}
		_, err = r.Remote.Call(ctx, p.Request(t, "join-start"))
		if err != nil {
			return result, fmt.Errorf("join %s: %w; retained plan resumes owned container", t.Name, err)
		}
		rec.Join.Nodes[t.Name] = provision.JoinNode{Phase: "syncing"}
		if err = save(); err != nil {
			return
		}
	}
	for {
		if err = r.scope(ctx, p, rec); err != nil {
			return
		}
		all := true
		var failures []error
		for _, t := range p.Targets {
			o, e := r.Remote.Call(ctx, p.Request(t, "join-status"))
			if e != nil {
				failures = append(failures, fmt.Errorf("%s: %w", t.Name, e))
				continue
			}
			n := provision.JoinNode{Phase: "syncing", ObservedAt: time.Now().UTC()}
			if o.Core != nil {
				n.ContainerID = o.Core.ContainerID
				n.Height = o.Core.Height
			}
			if ready(p, o) {
				n.Phase = "ready"
			} else {
				all = false
			}
			rec.Join.Nodes[t.Name] = n
		}
		if len(failures) > 0 {
			return result, errors.Join(failures...)
		}
		if all {
			rec.Join.Phase = "joined"
		}
		if err = save(); err != nil {
			return
		}
		if all {
			return rec, nil
		}
		if r.Progress != nil {
			r.Progress("waiting for every target to sync, prove checkpoint and observe fresh ChainLocks")
		}
		if r.Wait != nil {
			err = r.Wait(ctx)
		} else {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(15 * time.Second):
			}
		}
		if err != nil {
			return
		}
	}
}
