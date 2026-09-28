package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
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
	// Hosts are independent: probe, prepare and verify them concurrently. SSH
	// work runs in the pool; this goroutine records each result and saves the
	// journal, so checkpoints stay serial and a failed save stops the batch.
	sayMu := sync.Mutex{}
	say := func(s string) { sayMu.Lock(); defer sayMu.Unlock(); report(s) }
	record := func(t Target, n provision.BootstrapNode) error { r.Bootstrap.Nodes[t.Name] = n; return save() }
	unknown := func() provision.BootstrapNode {
		return provision.BootstrapNode{Phase: "unknown", ObservedAt: time.Now().UTC()}
	}
	for _, t := range p.Targets {
		r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "checking"}
	}
	if err = save(); err != nil {
		return
	}
	if err = fleet(ctx, p.Targets, func(ctx context.Context, t Target) (provision.BootstrapNode, error) {
		h := hosts[t.Name]
		say("checking " + t.Name)
		observation, probeErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "probe")
		if probeErr != nil {
			return unknown(), fmt.Errorf("preflight %s: %w", t.Name, probeErr)
		}
		if observation.Ready {
			return ready(observation), nil
		}
		return provision.BootstrapNode{Phase: "pending", ObservedAt: time.Now().UTC()}, nil
	}, record); err != nil {
		return
	}
	var pending []Target
	for _, t := range p.Targets {
		if r.Bootstrap.Nodes[t.Name].Phase != "ready" {
			pending = append(pending, t)
			r.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "preparing"}
		}
	}
	if len(pending) > 0 {
		// No host is mutated unless this claim is still ours.
		if err = save(); err != nil {
			return
		}
	}
	if err = fleet(ctx, pending, func(ctx context.Context, t Target) (provision.BootstrapNode, error) {
		h := hosts[t.Name]
		say("preparing " + t.Name)
		if _, applyErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "apply"); applyErr != nil {
			return unknown(), fmt.Errorf("prepare %s (resume rechecks host state): %w", t.Name, applyErr)
		}
		// Independent readback after mutation; do not trust the apply response alone.
		observation, probeErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "probe")
		if probeErr != nil || !observation.Ready {
			if probeErr == nil {
				probeErr = errors.New("runtime/images not ready after preparation")
			}
			return unknown(), fmt.Errorf("verify %s: %w", t.Name, probeErr)
		}
		return ready(observation), nil
	}, record); err != nil {
		return
	}
	// Read the whole fleet once more: no earlier target disappears behind later work.
	if _, err = provision.RunningTargets(ctx, p.Compute, r, cloud); err != nil {
		return
	}
	if err = fleet(ctx, p.Targets, func(ctx context.Context, t Target) (provision.BootstrapNode, error) {
		h := hosts[t.Name]
		observation, probeErr := Observe(ctx, remote, h.endpoint, p, t, h.id, "probe")
		if probeErr != nil || !observation.Ready {
			if probeErr == nil {
				probeErr = errors.New("runtime/images no longer ready")
			}
			return unknown(), fmt.Errorf("final verification %s: %w", t.Name, probeErr)
		}
		return ready(observation), nil
	}, record); err != nil {
		return
	}
	r.Bootstrap.Phase = "hosts-ready"
	if err = save(); err != nil {
		return
	}
	report("all hosts prepared and images verified; no Dash services started; application health unknown")
	return
}

// parallelHosts bounds concurrent SSH sessions. Preparing a host (runtime
// install and image pulls) takes about a minute, so a fleet prepares in about
// the time of one host.
const parallelHosts = 16

// fleet runs work for every target, at most parallelHosts at a time, and passes
// each result to record on the calling goroutine in completion order. A new
// target starts only after the previous result was recorded, so no host is
// touched past a failed checkpoint (a lost claim). A work error is kept and the
// others still finish; each host's state is rechecked on resume. After a record
// error nothing further starts; work already running finishes unrecorded.
func fleet(ctx context.Context, targets []Target, work func(context.Context, Target) (provision.BootstrapNode, error), record func(Target, provision.BootstrapNode) error) error {
	type reply struct {
		t   Target
		n   provision.BootstrapNode
		err error
	}
	replies := make(chan reply)
	next, active := 0, 0
	start := func() {
		t := targets[next]
		next++
		active++
		go func() {
			n, err := work(ctx, t)
			replies <- reply{t, n, err}
		}()
	}
	for next < len(targets) && active < parallelHosts {
		start()
	}
	var errs []error
	var recordErr error
	for active > 0 {
		x := <-replies
		active--
		if x.err != nil {
			errs = append(errs, x.err)
		}
		if recordErr != nil {
			continue
		}
		if recordErr = record(x.t, x.n); recordErr == nil && next < len(targets) && ctx.Err() == nil {
			start()
		}
	}
	if recordErr != nil {
		return errors.Join(append([]error{recordErr}, errs...)...)
	}
	if next < len(targets) {
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

func ready(o Observation) provision.BootstrapNode {
	return provision.BootstrapNode{Phase: "ready", ObservedAt: time.Now().UTC(), DockerVersion: o.DockerVersion, ComposeVersion: o.ComposeVersion}
}
