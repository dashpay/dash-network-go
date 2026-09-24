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
	return node.Request{Target: t, Action: action, Context: node.Context{PlanID: p.ID, ComputePlanID: p.Bootstrap.Compute.ID, BootstrapID: p.Bootstrap.ID, Network: p.Bootstrap.Compute.Network.Metadata.Name, CoreNetwork: p.CoreNetwork, PlatformChainID: p.PlatformChainID, GenesisTime: p.GenesisTime.Format(time.RFC3339Nano), InitialProtocolVersion: p.InitialProtocolVersion, MiningIntervalSeconds: p.MiningIntervalSeconds, CorePeers: p.PeerAddresses(), Ports: node.DefaultPorts}}
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
	if e.r.Bootstrap == nil || e.r.Bootstrap.PlanID != p.Bootstrap.ID || e.r.Bootstrap.Phase != "hosts-ready" {
		return result, errors.New("bootstrap changed before claim")
	}
	if e.r.Deployment != nil && e.r.Deployment.PlanID != p.ID {
		return result, errors.New("different deployment already owns this network; no implicit adoption or reset")
	}
	if stop && e.r.Deployment == nil {
		return result, errors.New("cannot stop a deployment that has never started")
	}
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
		if err = e.each(p.Targets, "stop", nil, nil); err != nil {
			return
		}
		e.r.Deployment.Phase = "stopped"
		err = e.stage("stopped")
		return
	}
	err = e.deploy()
	return
}

// Bound concurrency while checkpointing on one goroutine. Every scheduled target
// reports a result; one failed host never silently drops the remainder.
func (e *execution) each(targets []node.Target, action string, prepare func(*node.Request), accept func(node.Target, node.Observation) error) error {
	type reply struct {
		t   node.Target
		o   node.Observation
		err error
	}
	batchCtx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	work := make(chan node.Target)
	results := make(chan reply, len(targets))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				q := e.p.Request(t, action)
				if prepare != nil {
					prepare(&q)
				}
				o, err := e.runner.Remote.Call(batchCtx, q)
				results <- reply{t, o, err}
			}
		}()
	}
	go func() {
		for _, t := range targets {
			work <- t
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
	q := e.p.Request(t, action)
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
	if _, err = e.call(p.Miner(), "mine-start", func(q *node.Request) { q.PayoutAddress = d.PayoutAddress }); err != nil {
		return err
	}
	if err = e.stage("quorums"); err != nil {
		return err
	}
	for {
		ready := true
		minLock := int64(0)
		err = e.each(p.Targets, "core-status", nil, func(t node.Target, o node.Observation) error {
			if err := e.core(t, o); err != nil {
				return err
			}
			if err := coreHealthy(t, d.Nodes[t.Name], o.Core); err != nil {
				ready = false
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
			return e.stage("ready")
		}
		e.report("waiting for advancing, consistent consensus and DAPI: " + strings.Join(health.Problems, "; "))
		if err = e.wait(); err != nil {
			return fmt.Errorf("application verification incomplete: %w", err)
		}
	}
}
func (e *execution) peers() []node.Peer {
	var peers []node.Peer
	for _, t := range e.p.Validators() {
		n := e.r.Deployment.Nodes[t.Name]
		peers = append(peers, node.Peer{Name: t.Name, Address: t.PeerAddress, NodeID: n.PlatformNodeID, OperatorPublicKey: n.OperatorPublicKey, ProTxHash: n.ProTxHash})
	}
	return peers
}
func coreHealthy(t node.Target, n provision.DeploymentNode, c *node.Core) error {
	if c == nil || c.IBD || c.Peers < 1 || c.Height < c.Headers || c.ChainLockHeight < 1 || c.ChainLockHeight < c.Height-12 {
		return errors.New("Core sync/peers/ChainLock not ready")
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
