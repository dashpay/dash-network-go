package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
)

type Runner struct {
	Identity       inventory.STS
	Cloud          provision.EC2
	Store          bootstrap.Store
	Remote         node.Backend
	Owner, Version string
	Progress       func(string)
	// ObservationWindow is the minimum interval between health samples. Zero
	// retains the default; it never relaxes identity, quorum or agreement gates.
	ObservationWindow time.Duration
	// Tests inject a clock wait; production uses context-aware timers.
	Wait func(context.Context) error
}

type execution struct {
	runner Runner
	p      Plan
	r      provision.Record
	ctx    context.Context
}

func (p Plan) Request(t node.Target, action string) node.Request {
	return node.Request{
		Target: t, Action: action,
		Context: node.Context{
			PlanID: p.ID, ComputePlanID: p.Bootstrap.Compute.ID, BootstrapID: p.Bootstrap.ID,
			Network:     p.Bootstrap.Compute.Network.Metadata.Name,
			CoreNetwork: p.CoreNetwork, PlatformChainID: p.PlatformChainID,
			GenesisTime:            p.GenesisTime.Format(time.RFC3339Nano),
			InitialProtocolVersion: p.InitialProtocolVersion,
			MiningIntervalSeconds:  p.MiningIntervalSeconds, MiningNodeName: p.Miner().Name,
			CorePeers: p.PeerAddresses(), Ports: node.DefaultPorts,
		},
	}
}
func (e *execution) report(s string) {
	if e.runner.Progress != nil {
		e.runner.Progress(s)
	}
}
func (e *execution) save() error {
	e.r.UpdatedAt = time.Now().UTC()
	e.r.Revision++
	return e.runner.Store.Save(e.ctx, e.r, e.runner.Owner)
}
func (e *execution) stage(name string) error {
	e.r.Deployment.Stage = name
	e.report(name)
	return e.save()
}
func (e *execution) wait() error {
	if e.runner.Wait != nil {
		return e.runner.Wait(e.ctx)
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-e.ctx.Done():
		return e.ctx.Err()
	case <-timer.C:
		return nil
	}
}
func liveScope(ctx context.Context, p Plan, r provision.Record, cloud provision.EC2) error {
	live, err := provision.RunningTargets(ctx, p.Bootstrap.Compute, r, cloud)
	if err != nil {
		return err
	}
	for _, t := range p.Targets {
		v := live[t.Name]
		ip := aws.ToString(v.PrivateIpAddress)
		if p.Bootstrap.Access.Address == "public" {
			ip = aws.ToString(v.PublicIpAddress)
		}
		if aws.ToString(v.InstanceId) != t.InstanceID || ip != t.SSHAddress || aws.ToString(v.PrivateIpAddress) != t.PeerAddress {
			return fmt.Errorf("instance/address drift at %s; deployment plan is bound to exact hosts", t.Name)
		}
	}
	return nil
}

// Execute holds the existing compute claim for the complete operation. A lost
// runner leaves a visible claim; operator recovery never steals a timed-out lease.
func (r Runner) Execute(ctx context.Context, p Plan, stop bool) (result provision.Record, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	if r.Remote == nil || r.Owner == "" {
		return result, errors.New("authenticated backend and runner identity required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, errors.New("deployment requires a deadline")
	}
	if err = provision.VerifyAccount(ctx, p.Bootstrap.Compute.Network.AWS.AccountID, r.Identity); err != nil {
		return
	}
	prior, _, err := r.Store.Read(ctx, p.Bootstrap.Compute)
	if err != nil {
		return result, err
	}
	if err = prior.Validate(p.Bootstrap.Compute); err != nil {
		return
	}
	if prior.Join != nil {
		return result, errors.New("allocation belongs to an existing chain join, not genesis lifecycle")
	}
	if prior.Upgrade != nil && prior.Upgrade.Phase != "complete" {
		return result, errors.New("unfinished upgrade owns runtime intent; resume that upgrade before deploy/stop")
	}
	if _, err = effectiveImages(p, prior); err != nil {
		return
	}
	if prior.Bootstrap == nil || prior.Bootstrap.PlanID != p.Bootstrap.ID || prior.Bootstrap.Phase != "hosts-ready" {
		return result, errors.New("finish exact node bootstrap first")
	}
	record, err := r.Store.Acquire(ctx, p.Bootstrap.Compute, r.Owner)
	if err != nil {
		return result, err
	}
	e := execution{runner: r, p: p, r: record, ctx: ctx}
	changed := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err != nil && changed {
			e.r.Deployment.Phase = "interrupted"
			e.r.LastError = node.SafeError(err)
			e.r.UpdatedAt = time.Now().UTC()
			e.r.Revision++
			if saveErr := r.Store.Save(cleanup, e.r, r.Owner); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("save interruption: %w", saveErr))
			}
		}
		if releaseErr := r.Store.Release(cleanup, p.Bootstrap.Compute, r.Owner); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
		result = e.r
	}()
	if err = e.r.Validate(p.Bootstrap.Compute); err != nil {
		return
	}
	if e.r.Upgrade != nil && e.r.Upgrade.Phase != "complete" {
		return result, errors.New("upgrade changed before claim; resume that upgrade")
	}
	if _, err = effectiveImages(p, e.r); err != nil {
		return
	}
	if e.r.Bootstrap == nil || e.r.Bootstrap.PlanID != p.Bootstrap.ID || e.r.Bootstrap.Phase != "hosts-ready" {
		return result, errors.New("bootstrap changed before claim")
	}
	if e.r.Deployment != nil && e.r.Deployment.PlanID != p.ID {
		return result, errors.New("different deployment already owns this network; no implicit adoption or reset")
	}
	if stop && e.r.Deployment == nil {
		return result, errors.New("cannot stop a deployment that has never started")
	}
	verifyFirst := e.r.Deployment != nil && e.r.Deployment.GenesisCoreHeight > 0 &&
		(e.r.Deployment.Stage == "health" || e.r.Deployment.Stage == "ready")
	if e.r.Deployment == nil {
		e.r.Deployment = &provision.DeploymentProgress{PlanID: p.ID, Nodes: map[string]provision.DeploymentNode{}}
	}
	e.r.Deployment.Phase = "deploying"
	e.r.Deployment.ObservedAt = time.Time{}
	for _, t := range p.Targets {
		n := e.r.Deployment.Nodes[t.Name]
		n.Phase = "pending"
		n.ObservedAt = time.Time{}
		e.r.Deployment.Nodes[t.Name] = n
	}
	e.r.LastRunner = r.Owner
	e.r.CLIVersion = r.Version
	e.r.LastError = ""
	changed = true
	if err = e.stage("preflight"); err != nil {
		return
	}
	if err = liveScope(ctx, p, e.r, r.Cloud); err != nil {
		return
	}
	if err = e.each(p.Targets, "inspect", nil, nil); err != nil {
		return
	}
	if stop {
		if err = e.stage("stopping"); err != nil {
			return
		}
		// Freeze block production before withdrawing validators. Otherwise a
		// wallet at the end of the target list can keep mining DKG rounds while
		// the earlier batches are intentionally offline.
		if err = e.each([]node.Target{p.Miner()}, "stop", nil, nil); err != nil {
			return
		}
		var remaining []node.Target
		for _, t := range p.Targets {
			if t.Name != p.Miner().Name {
				remaining = append(remaining, t)
			}
		}
		if err = e.each(remaining, "stop", nil, nil); err != nil {
			return
		}
		e.r.Deployment.Phase = "stopped"
		err = e.stage("stopped")
		return
	}
	if verifyFirst {
		// An interrupted final check must not repeat wallet/registration work
		// when the exact intended fleet is already healthy. This is fresh
		// two-sample evidence, not trust in a cached journal success flag.
		if err = e.stage("health"); err != nil {
			return
		}
		var health Health
		health, err = r.Doctor(ctx, p, e.r)
		if err != nil {
			return
		}
		if health.Healthy {
			err = e.acceptHealth(health)
			return
		}
		e.report("existing fleet is not yet healthy; reconciling the original deployment")
	}
	err = e.deploy()
	return
}

// Bound concurrency while checkpointing on one goroutine. Every scheduled target
// reports a result; one failed host never silently drops the remainder.
func (e *execution) each(targets []node.Target, action string, prepare func(*node.Request), accept func(node.Target, node.Observation) error) error {
	type job struct {
		t node.Target
		q node.Request
	}
	type reply struct {
		t   node.Target
		o   node.Observation
		err error
	}
	batchCtx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	// Capture immutable intent before any checkpoint can change the journal.
	// Workers must not copy/read e.r while the result loop updates it.
	jobs := make([]job, 0, len(targets))
	for _, t := range targets {
		q := runtimeRequest(e.p, e.r, t, action)
		if prepare != nil {
			prepare(&q)
		}
		jobs = append(jobs, job{t, q})
	}
	work := make(chan job)
	results := make(chan reply, len(targets))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				o, err := e.runner.Remote.Call(batchCtx, j.q)
				results <- reply{j.t, o, err}
			}
		}()
	}
	go func() {
		for _, j := range jobs {
			work <- j
		}
		close(work)
		wg.Wait()
		close(results)
	}()
	var failures []error
	for result := range results {
		err := result.err
		if err == nil && accept != nil {
			err = accept(result.t, result.o)
		}
		n := e.r.Deployment.Nodes[result.t.Name]
		if err != nil {
			n.Phase = "unknown"
			failures = append(failures, fmt.Errorf("%s %s: %w", action, result.t.Name, err))
		} else {
			e.report(action + " " + result.t.Name + " verified")
		}
		n.ObservedAt = time.Now().UTC()
		e.r.Deployment.Nodes[result.t.Name] = n
		if saveErr := e.save(); saveErr != nil {
			cancel()
			failures = append(failures, saveErr)
		}
	}
	return errors.Join(failures...)
}
func (e *execution) call(t node.Target, action string, prepare func(*node.Request)) (node.Observation, error) {
	q := runtimeRequest(e.p, e.r, t, action)
	if prepare != nil {
		prepare(&q)
	}
	o, err := e.runner.Remote.Call(e.ctx, q)
	if err != nil {
		return o, fmt.Errorf("%s %s: %w", action, t.Name, err)
	}
	return o, nil
}
func (e *execution) core(t node.Target, o node.Observation) error {
	if o.Core == nil || len(o.Core.Genesis) != 64 || o.Core.Height < 1 || len(o.Core.ContainerID) != 64 {
		return errors.New("missing Core identity/readiness evidence")
	}
	d := e.r.Deployment
	if d.CoreGenesis == "" {
		d.CoreGenesis = o.Core.Genesis
	} else if d.CoreGenesis != o.Core.Genesis {
		return errors.New("Core genesis mismatch")
	}
	n := d.Nodes[t.Name]
	n.CoreHeight = o.Core.Height
	n.CoreContainerID = o.Core.ContainerID
	n.Phase = "core-ready"
	d.Nodes[t.Name] = n
	return nil
}
func unchanged(previous, actual string) bool {
	return actual != "" && (previous == "" || previous == actual)
}
func (e *execution) deploy() error {
	p, d := e.p, e.r.Deployment
	if err := e.stage("core-start"); err != nil {
		return err
	}
	if err := e.each(p.Targets, "core-start", nil, e.core); err != nil {
		return err
	}
	if err := e.stage("identities"); err != nil {
		return err
	}
	wallet, err := e.call(p.Wallet(), "wallet", nil)
	if err != nil {
		return err
	}
	if !unchanged(d.PayoutAddress, wallet.PayoutAddress) || !unchanged(d.SporkAddress, wallet.SporkAddress) {
		return errors.New("wallet identity changed")
	}
	d.PayoutAddress = wallet.PayoutAddress
	d.SporkAddress = wallet.SporkAddress
	if err = e.save(); err != nil {
		return err
	}
	if err = e.each(p.Validators(), "identity", nil, func(t node.Target, o node.Observation) error {
		n := d.Nodes[t.Name]
		if len(o.OperatorPublicKey) != 96 || len(o.PlatformNodeID) != 40 || !unchanged(n.OperatorPublicKey, o.OperatorPublicKey) || !unchanged(n.PlatformNodeID, o.PlatformNodeID) {
			return errors.New("validator identity changed or missing")
		}
		n.OperatorPublicKey = o.OperatorPublicKey
		n.PlatformNodeID = o.PlatformNodeID
		n.Phase = "identified"
		d.Nodes[t.Name] = n
		return nil
	}); err != nil {
		return err
	}
	if err = e.stage("core-finalize"); err != nil {
		return err
	}
	if err = e.each(p.Targets, "core-finalize", func(q *node.Request) { q.SporkAddress = d.SporkAddress }, e.core); err != nil {
		return err
	}
	if _, err = e.call(p.Wallet(), "wallet", nil); err != nil {
		return err
	}
	if err = e.stage("registrations"); err != nil {
		return err
	}
	for _, t := range p.Validators() {
		// Funding/registration is deliberately serial on one wallet. Each signed
		// transaction is durable on that wallet before sendrawtransaction.
		if _, err = e.call(p.Wallet(), "fund", func(q *node.Request) { q.RequiredBalance = 4001 }); err != nil {
			return err
		}
		n := d.Nodes[t.Name]
		peer := node.Peer{Name: t.Name, Address: t.PeerAddress, NodeID: n.PlatformNodeID, OperatorPublicKey: n.OperatorPublicKey}
		var o node.Observation
		o, err = e.call(p.Wallet(), "register", func(q *node.Request) { q.Registration = &peer; q.RequiredConfirmations = 1 })
		if err != nil {
			return err
		}
		if len(o.ProTxHash) != 64 || o.Confirmations < 1 || !unchanged(n.ProTxHash, o.ProTxHash) {
			return errors.New("registration identity/confirmation mismatch")
		}
		n.ProTxHash = o.ProTxHash
		n.Phase = "registered"
		d.Nodes[t.Name] = n
		if err = e.save(); err != nil {
			return err
		}
		e.report("registered " + t.Name)
	}
	if _, err = e.call(p.Wallet(), "activate", nil); err != nil {
		return err
	}
	// With <=3 ordinary peers Core requires a quiet interval before mnsync
	// finishes. Starting ten-second mining first can reset that timer forever.
	// Keep Core running; stop only the owned miner if a resumed run needs quiet.
	if err = e.stage("core-sync"); err != nil {
		return err
	}
	paused := false
	for {
		ready := true
		if err = e.each(p.Targets, "core-status", nil, func(t node.Target, o node.Observation) error {
			if err := e.core(t, o); err != nil {
				return err
			}
			if !o.Core.Synced || o.Core.IBD || o.Core.Headers > o.Core.Height || o.Core.Peers == 0 {
				ready = false
			}
			return nil
		}); err != nil {
			return err
		}
		if ready {
			break
		}
		if !paused {
			if _, err = e.call(p.Miner(), "mine-pause", nil); err != nil {
				return err
			}
			paused = true
		}
		e.report("waiting for masternode sync with mining paused; Core remains running")
		if err = e.wait(); err != nil {
			return err
		}
	}
	if _, err = e.call(p.Miner(), "mine-start", func(q *node.Request) { q.PayoutAddress = d.PayoutAddress }); err != nil {
		return err
	}
	if err = e.stage("quorums"); err != nil {
		return err
	}
	for {
		ready := true
		var waiting []string
		minLock := int64(0)
		err = e.each(p.Targets, "core-status", nil, func(t node.Target, o node.Observation) error {
			if err := e.core(t, o); err != nil {
				return err
			}
			if err := coreHealthy(t, d.Nodes[t.Name], o.Core, p.Miner().Name); err != nil {
				ready = false
				waiting = append(waiting, fmt.Sprintf("%s: %s (Core height %d)", t.Name, err, o.Core.Height))
			}
			if minLock == 0 || o.Core.ChainLockHeight < minLock {
				minLock = o.Core.ChainLockHeight
			}
			return nil
		})
		if err != nil {
			return err
		}
		if ready && minLock > 0 {
			if d.GenesisCoreHeight == 0 {
				d.GenesisCoreHeight = minLock
				if err = e.save(); err != nil {
					return err
				}
			}
			break
		}
		e.report("waiting for Core readiness: " + strings.Join(waiting, "; "))
		if err = e.wait(); err != nil {
			return fmt.Errorf("waiting for READY masternodes, all devnet quorums and ChainLocks: %w", err)
		}
	}
	if err = e.stage("platform-start"); err != nil {
		return err
	}
	peers := e.peers()
	if err = e.each(p.Validators(), "platform-start", func(q *node.Request) { q.Peers = peers; q.GenesisCoreHeight = d.GenesisCoreHeight }, func(t node.Target, o node.Observation) error {
		n := d.Nodes[t.Name]
		n.Phase = "platform-started"
		d.Nodes[t.Name] = n
		return nil
	}); err != nil {
		return err
	}
	if err = e.stage("health"); err != nil {
		return err
	}
	for {
		health, probeErr := e.runner.Doctor(e.ctx, p, e.r)
		if probeErr != nil {
			return probeErr
		}
		if health.Healthy {
			return e.acceptHealth(health)
		}
		e.report("waiting for advancing, consistent consensus and DAPI: " + strings.Join(health.Problems, "; "))
		if err = e.wait(); err != nil {
			return fmt.Errorf("application verification incomplete: %w", err)
		}
	}
}

func (e *execution) acceptHealth(health Health) error {
	d := e.r.Deployment
	for name, v := range health.Nodes {
		n := d.Nodes[name]
		n.Phase = "ready"
		n.ObservedAt = health.ObservedAt
		n.CoreHeight = v.CoreHeight
		n.PlatformHeight = v.PlatformHeight
		d.Nodes[name] = n
	}
	d.Phase = "network-ready"
	d.ObservedAt = health.ObservedAt
	// A completed recovery must not present the preceding failure as current.
	// Failed attempts remain in their retained operation logs/checkpoints.
	e.r.LastError = ""
	return e.stage("ready")
}
func (e *execution) peers() []node.Peer {
	var peers []node.Peer
	for _, t := range e.p.Validators() {
		n := e.r.Deployment.Nodes[t.Name]
		peers = append(peers, node.Peer{Name: t.Name, Address: t.PeerAddress, NodeID: n.PlatformNodeID, OperatorPublicKey: n.OperatorPublicKey, ProTxHash: n.ProTxHash})
	}
	return peers
}
func coreHealthy(t node.Target, n provision.DeploymentNode, c *node.Core, miner string) error {
	if c == nil {
		return errors.New("Core observation missing")
	}
	if !c.Synced || c.IBD || c.Peers < 1 || c.Height < c.Headers {
		return fmt.Errorf("Core not synchronized (height=%d headers=%d peers=%d mnsync=%t ibd=%t)", c.Height, c.Headers, c.Peers, c.Synced, c.IBD)
	}
	if c.ChainLockHeight < 1 || c.ChainLockHeight < c.Height-12 {
		return fmt.Errorf("ChainLock not ready/fresh (locked=%d tip=%d)", c.ChainLockHeight, c.Height)
	}
	if t.Name == miner && (c.Mining == nil || !c.Mining.Running || len(c.Mining.ContainerID) != 64) {
		return errors.New("persistent miner unavailable")
	}
	for _, name := range []string{"llmq_devnet", "llmq_devnet_dip0024", "llmq_devnet_platform"} {
		if c.Quorums[name] < 1 {
			return errors.New("missing " + name)
		}
	}
	if t.Role == "validator" && (c.MasternodeState != "READY" || !strings.EqualFold(c.ProTxHash, n.ProTxHash)) {
		return errors.New("masternode not READY with registered identity")
	}
	return nil
}
