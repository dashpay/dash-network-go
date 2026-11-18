package managed

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dashpay/dash-network-go/internal/rollout"
)

func (p *Plan) WithRollout(options rollout.Options) error {
	p.Rollout = &options
	p.ID = ""
	p.ID = hash(*p)
	return p.Validate()
}
func (p Plan) validateRollout() error {
	if p.Rollout == nil {
		return nil
	}
	if err := p.Rollout.Validate(); err != nil {
		return err
	}
	if p.Operation != "upgrade" || p.Migration != nil {
		return errors.New("rollout options require an ordinary image upgrade, not a schema migration")
	}
	for _, name := range p.Rollout.Miners {
		found := false
		for _, t := range p.Snapshot.Fleet.Targets {
			if t.Name != name {
				continue
			}
			if p.Selects(t) || t.Role != "core" || t.MiningService != "dashd-generate-miner.service" || p.Snapshot.Fleet.ChainType != "testnet" {
				return errors.New("pause only explicit unselected owned Testnet miners")
			}
			o := p.Snapshot.Nodes[name]
			if o.Mining == nil || !o.Mining.Active || !digestRE.MatchString(o.Mining.ServiceHash) || o.Healthy(t) != nil {
				return errors.New("healthy active mining baseline required")
			}
			found = true
		}
		if !found {
			return fmt.Errorf("mining target %s missing from reviewed snapshot", name)
		}
	}
	return nil
}

// Reuse the existing identity-verified miner lease protocol without opting the
// upgrade into the legacy Platform/config migration. Only mining calls use it.
func (p Plan) miningPolicy() *Migration {
	if p.Rollout != nil && p.Rollout.PauseMining {
		return &Migration{Kind: LegacyTestnetV5, ReadinessDriven: true, FleetMiningPause: true, Miners: slices.Clone(p.Rollout.Miners)}
	}
	return p.Migration
}

func canWithdrawBatch(s Snapshot, p Plan, targets []Target) error {
	names := map[string]bool{}
	for _, t := range targets {
		for _, component := range []string{"core", "drive", "tenderdash"} {
			if p.Images[t.Name][component] != "" && t.Role == "validator" {
				names[t.Name] = true
			}
		}
	}
	// Exclude every member of the proposed batch from remaining voting power.
	s.Nodes = maps.Clone(s.Nodes)
	for name := range names {
		o := s.Nodes[name]
		o.Chain.MasternodeState = "WITHDRAWING"
		s.Nodes[name] = o
	}
	for _, t := range targets {
		if names[t.Name] {
			if err := CanWithdraw(s, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// resumeAdmission protects a journaled batch that was never withdrawn. The
// journal records intent only: a member still healthy on its previous images
// would be taken down by the resume, in a fleet that may have degraded since
// the batch was admitted. A member that is down or mid-replacement always
// takes precedence, and the batch is then reconciled exactly as journaled.
func (r Runner) resumeAdmission(ctx context.Context, p Plan, targets []Target) error {
	var fresh []Target
	for _, t := range targets {
		o, err := r.Remote.Call(ctx, request(p.Snapshot.Fleet, t, "observe"))
		if err != nil || o.Healthy(t) != nil {
			return nil
		}
		for component, pin := range p.Images[t.Name] {
			// By digest, as verify does: Docker may list the same image under
			// another spelling of its repository.
			_, want, _ := strings.Cut(pin, "@")
			if !slices.ContainsFunc(o.Components[component].Digests, func(d string) bool {
				_, got, ok := strings.Cut(d, "@")
				return ok && got == want
			}) {
				fresh = append(fresh, t)
				break
			}
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	if err := VerifyCloud(ctx, p.Snapshot.Fleet, r.Identity, r.Cloud); err != nil {
		return err
	}
	h, err := r.withdrawalHealth(ctx, p)
	if err != nil {
		return err
	}
	if !r.rolloutHealthy(h, p) {
		return errors.New("journaled batch not resumed: fleet unhealthy before batch withdrawal")
	}
	if err = canWithdrawBatch(h.Snapshot, p, fresh); err != nil {
		return fmt.Errorf("journaled batch not resumed: %w", err)
	}
	return nil
}

// Pending intent is persisted for the ENTIRE batch before any SSH apply. The
// caller alone writes the journal; failed/lost replies retain every pending
// target, and resume reconciles that exact batch before admitting another.
func (r Runner) executeRollout(ctx context.Context, p Plan, record *Record, save func() error) (err error) {
	operationCtx := ctx
	owned := record.MiningPhase == "pausing" || record.MiningPhase == "paused"
	var lease *fleetLease
	var stop func() error
	defer func() {
		if stop != nil {
			err = errors.Join(err, stop())
		}
		if owned {
			record.MiningPhase = "resuming"
			cleanup := r.resumeMining(p)
			if cleanup == nil {
				record.MiningPhase = "resumed"
			}
			err = errors.Join(err, cleanup)
		}
	}()
	pending := false
	for _, t := range p.Snapshot.Fleet.Targets {
		pending = pending || !record.Completed[t.Name]
	}
	if p.Rollout.PauseMining && pending {
		owned = true
		record.MiningPhase = "pausing"
		if err = save(); err != nil {
			return
		}
		for _, t := range miningTargets(p) {
			if _, err = r.miningCall(ctx, p, t, "mining_pause"); err != nil {
				return
			}
		}
		lease = &fleetLease{r: r, p: p}
		if _, _, err = lease.check(ctx, false); err != nil {
			return
		}
		ctx, stop = lease.keep(ctx, time.Minute)
		r.fleetPaused = true
		record.MiningPhase = "paused"
		if err = save(); err != nil {
			return
		}
		r.report("owned mining paused for the upgrade")
	}
	for {
		var targets []Target
		if len(record.PendingTargets) > 0 {
			for _, name := range record.PendingTargets {
				for _, t := range p.Snapshot.Fleet.Targets {
					if t.Name == name {
						targets = append(targets, t)
					}
				}
			}
			r.batchTargets = slices.Clone(record.PendingTargets)
			if err = r.resumeAdmission(ctx, p, targets); err != nil {
				return
			}
		} else {
			for _, t := range p.Snapshot.Fleet.Targets {
				if !record.Completed[t.Name] {
					targets = append(targets, t)
					if len(targets) == p.Rollout.Concurrency {
						break
					}
				}
			}
			if len(targets) == 0 {
				break
			}
			r.batchTargets = nil
			for _, t := range targets {
				r.batchTargets = append(r.batchTargets, t.Name)
			}
			if err = VerifyCloud(ctx, p.Snapshot.Fleet, r.Identity, r.Cloud); err != nil {
				return
			}
			var h Health
			if h, err = r.withdrawalHealth(ctx, p); err != nil {
				return
			}
			if !r.rolloutHealthy(h, p) {
				return errors.New("fleet unhealthy before batch withdrawal")
			}
			for {
				err = canWithdrawBatch(h.Snapshot, p, targets)
				if err == nil {
					break
				}
				if len(targets) == 1 {
					return
				}
				targets = targets[:len(targets)-1]
			}
			record.PendingTargets = nil
			for _, t := range targets {
				record.PendingTargets = append(record.PendingTargets, t.Name)
			}
			record.Phase = "applying"
			if err = save(); err != nil {
				return
			}
		}
		r.batchTargets = nil
		for _, t := range targets {
			r.batchTargets = append(r.batchTargets, t.Name)
		}
		var checkpoint int64
		var checkpointHash string
		if lease != nil {
			if checkpoint, checkpointHash, err = lease.check(ctx, false); err != nil {
				return
			}
		}
		for _, t := range targets {
			r.report("managed target " + t.Name + " applying")
		}
		r.report("managed-applying")
		type result struct {
			name string
			err  error
			line string
		}
		results := make(chan result, len(targets))
		var wg sync.WaitGroup
		for _, t := range targets {
			q := request(p.Snapshot.Fleet, t, "apply")
			baseline := p.Snapshot.Nodes[t.Name]
			q.Expected = &baseline
			q.Pins = p.Images[t.Name]
			q.Operation = p.Operation
			q.OperationID = p.ID
			q.CoreCheckpointHeight, q.CoreCheckpointHash = checkpoint, checkpointHash
			wg.Add(1)
			go func(t Target, q Request) {
				defer wg.Done()
				worker := r
				worker.Progress = func(line string) { results <- result{line: line} }
				_, e := r.Remote.Call(ctx, q)
				if e == nil {
					e = worker.awaitReady(ctx, p.Snapshot.Fleet, t)
				}
				if e == nil && checkpoint > 0 {
					check := request(p.Snapshot.Fleet, t, "observe")
					check.CoreCheckpointHeight = checkpoint
					o, ce := r.Remote.Call(ctx, check)
					e = ce
					if e == nil && (o.Chain.CoreCheckpointHash != checkpointHash || o.Chain.CoreHeight < checkpoint) {
						e = errors.New("restarted node lost paused Core checkpoint")
					}
				}
				results <- result{name: t.Name, err: e}
			}(t, q)
		}
		for completed := 0; completed < len(targets); {
			x := <-results
			if x.line != "" {
				r.report(x.line)
				continue
			}
			completed++
			if x.err != nil {
				err = errors.Join(err, fmt.Errorf("%s: %w", x.name, x.err))
			}
		}
		wg.Wait()
		if err != nil {
			return
		}
		if lease != nil {
			if _, _, err = lease.check(ctx, false); err != nil {
				return
			}
		}
		record.Phase = "verifying"
		if err = save(); err != nil {
			return
		}
		if err = r.verify(ctx, p, *record); err != nil {
			return
		}
		for _, t := range targets {
			record.Completed[t.Name] = true
		}
		record.PendingTargets = nil
		if err = save(); err != nil {
			return
		}
		for _, t := range targets {
			r.report("managed target " + t.Name + " applied")
		}
	}
	if owned || (p.Rollout.PauseMining && !pending) {
		if stop != nil {
			if err = stop(); err != nil {
				return
			}
			stop = nil
			ctx = operationCtx
		}
		record.MiningPhase = "resuming"
		if err = save(); err != nil {
			return
		}
		if err = r.resumeMining(p); err != nil {
			return
		}
		owned = false
		r.fleetPaused = false
		record.MiningPhase = "resumed"
		if err = save(); err != nil {
			return
		}
		r.report("mining resumed; verifying chain progression")
	}
	r.batchTargets = nil
	if err = r.verify(ctx, p, *record); err != nil {
		return
	}
	record.Phase = "complete"
	record.LastError = ""
	return save()
}

// Ordinary masternodes do not vote in Platform consensus. Their batches need
// only target readiness, not repeated scans of every unrelated fleet service.
// Validator batches additionally sample the current voting set for admission.
func (r Runner) rolloutSnapshot(p Plan) Snapshot {
	s := p.Snapshot
	s.Fleet.Targets = nil
	validators := false
	selected := func(t Target) bool {
		if len(r.batchTargets) > 0 {
			return slices.Contains(r.batchTargets, t.Name)
		}
		return p.Selects(t)
	}
	for _, t := range p.Snapshot.Fleet.Targets {
		validators = validators || selected(t) && t.Role == "validator"
	}
	for _, t := range p.Snapshot.Fleet.Targets {
		if selected(t) || validators && t.Role == "validator" {
			s.Fleet.Targets = append(s.Fleet.Targets, t)
		}
	}
	return s
}
func (r Runner) rolloutHealthy(h Health, p Plan) bool {
	if p.Rollout == nil || len(r.batchTargets) == 0 {
		return healthSelected(h, p)
	}
	for _, name := range r.batchTargets {
		if !h.Nodes[name].Healthy {
			return false
		}
	}
	return true
}
