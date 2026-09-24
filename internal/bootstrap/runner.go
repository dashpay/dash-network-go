package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/transport"
)

type Store interface {
	provision.Store
	Read(context.Context, provision.Plan) (provision.Record, string, error)
}

type host struct {
	endpoint transport.Endpoint
	id       string
}

// Execute shares the compute operation's owner claim and revision fencing.
// Preflight covers every host before mutation. Every resume probes real state,
// even for previously ready hosts; a journal checkpoint alone is not evidence.
func Execute(ctx context.Context, p Plan, identity inventory.STS, cloud provision.EC2, store Store, remote Remote, owner, version string, progress func(string)) (result provision.Record, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	if owner == "" || remote == nil {
		return result, errors.New("runner identity and authenticated transport required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, errors.New("bootstrap requires a deadline")
	}
	if err = provision.VerifyAccount(ctx, p.Compute.Network.AWS.AccountID, identity); err != nil {
		return
	}
	// Never create a new compute journal as a side effect of bootstrap.
	prior, _, err := store.Read(ctx, p.Compute)
	if err != nil {
		return result, err
	}
	if err = prior.Validate(p.Compute); err != nil {
		return
	}
	if prior.Phase != "compute-ready" {
		return result, errors.New("finish EC2 provisioning before node bootstrap")
	}
	r, err := store.Acquire(ctx, p.Compute, owner)
	if err != nil {
		return result, fmt.Errorf("claim bootstrap: %w", err)
	}
	changed := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err != nil && changed {
			r.Bootstrap.Phase = "interrupted"
			r.LastError = err.Error()
			if len(r.LastError) > 4096 {
				r.LastError = r.LastError[:4096]
			}
			r.UpdatedAt = time.Now().UTC()
			r.Revision++
			if saveErr := store.Save(cleanup, r, owner); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("save bootstrap interruption: %w", saveErr))
			}
		}
		if releaseErr := store.Release(cleanup, p.Compute, owner); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release runner claim: %w", releaseErr))
		}
		result = r
	}()
	if err = r.Validate(p.Compute); err != nil {
		return
	}
	if r.Phase != "compute-ready" {
		return result, errors.New("compute state changed before bootstrap claim")
	}
	if r.Bootstrap != nil && r.Bootstrap.PlanID != p.ID {
		return result, errors.New("network already has a different bootstrap plan; no automatic recipe or release replacement")
	}
	if r.Bootstrap == nil {
		r.Bootstrap = &provision.BootstrapProgress{PlanID: p.ID, Nodes: map[string]provision.BootstrapNode{}}
	}
	r.Bootstrap.Phase = "preparing"
	// Reset freshness for the whole fleet; an early failure must not leave old
	// ready entries masquerading as observations from this attempt.
	for _, t := range p.Targets {
		r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "pending"}
	}
	r.LastRunner = owner
	r.CLIVersion = version
	r.LastError = ""
	changed = true
	save := func() error { r.UpdatedAt = time.Now().UTC(); r.Revision++; return store.Save(ctx, r, owner) }
	report := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	if err = save(); err != nil {
		return
	}
	live, err := provision.RunningTargets(ctx, p.Compute, r, cloud)
	if err != nil {
		return result, err
	}
	hosts := map[string]host{}
	for _, t := range p.Targets {
		instance := live[t.Name]
		ip := aws.ToString(instance.PrivateIpAddress)
		if p.Access.Address == "public" {
			ip = aws.ToString(instance.PublicIpAddress)
		}
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil || parsed.IsLoopback() || parsed.IsUnspecified() || parsed.IsMulticast() || parsed.IsLinkLocalUnicast() {
			return result, fmt.Errorf("target %s lacks a usable %s IPv4 address", t.Name, p.Access.Address)
		}
		id := aws.ToString(instance.InstanceId)
		hosts[t.Name] = host{endpoint: transport.Endpoint{Address: ip, Port: p.Access.Port, HostAlias: p.HostAlias(id)}, id: id}
	}
	var preflightErrors []error
	for _, t := range p.Targets {
		if err = ctx.Err(); err != nil {
			return
		}
		r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "checking"}
		if err = save(); err != nil {
			return
		}
		h := hosts[t.Name]
		report("checking " + t.Name)
		observation, probeErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "probe")
		n := provision.BootstrapNode{Phase: "pending", ObservedAt: time.Now().UTC()}
		if probeErr != nil {
			n.Phase = "unknown"
			preflightErrors = append(preflightErrors, fmt.Errorf("preflight %s: %w", t.Name, probeErr))
		} else if observation.Ready {
			n = ready(observation)
		}
		r.Bootstrap.Nodes[t.Name] = n
		if err = save(); err != nil {
			return
		}
	}
	if len(preflightErrors) > 0 {
		return result, errors.Join(preflightErrors...)
	}
	for _, t := range p.Targets {
		if r.Bootstrap.Nodes[t.Name].Phase == "ready" {
			continue
		}
		if err = ctx.Err(); err != nil {
			return
		}
		h := hosts[t.Name]
		r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "preparing"}
		if err = save(); err != nil {
			return
		}
		report("preparing " + t.Name)
		_, applyErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "apply")
		if applyErr != nil {
			r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "unknown", ObservedAt: time.Now().UTC()}
			return result, fmt.Errorf("prepare %s (resume rechecks host state): %w", t.Name, applyErr)
		}
		// Independent readback after mutation; do not trust the apply response alone.
		observation, probeErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "probe")
		if probeErr != nil || !observation.Ready {
			r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "unknown", ObservedAt: time.Now().UTC()}
			if probeErr == nil {
				probeErr = errors.New("runtime/images not ready after preparation")
			}
			return result, fmt.Errorf("verify %s: %w", t.Name, probeErr)
		}
		r.Bootstrap.Nodes[t.Name] = ready(observation)
		if err = save(); err != nil {
			return
		}
	}
	// Read the whole fleet once more: no earlier target disappears behind later work.
	if _, err = provision.RunningTargets(ctx, p.Compute, r, cloud); err != nil {
		return
	}
	for _, t := range p.Targets {
		h := hosts[t.Name]
		observation, probeErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "probe")
		if probeErr != nil || !observation.Ready {
			r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "unknown", ObservedAt: time.Now().UTC()}
			if probeErr == nil {
				probeErr = errors.New("runtime/images no longer ready")
			}
			return result, fmt.Errorf("final verification %s: %w", t.Name, probeErr)
		}
		r.Bootstrap.Nodes[t.Name] = ready(observation)
		if err = save(); err != nil {
			return
		}
	}
	r.Bootstrap.Phase = "hosts-ready"
	if err = save(); err != nil {
		return
	}
	report("all hosts prepared and images verified; no Dash services started; application health unknown")
	return
}

func ready(o Observation) provision.BootstrapNode {
	return provision.BootstrapNode{Phase: "ready", ObservedAt: time.Now().UTC(), DockerVersion: o.DockerVersion, ComposeVersion: o.ComposeVersion}
}
