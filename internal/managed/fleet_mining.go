package managed

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

func fleetPause(p Plan) bool { return p.Migration != nil && p.Migration.FleetMiningPause }

// Serializes miner control calls with the renewer. Validator observations and
// restarts never hold this mutex; renewal continues during epoch holds and slow
// database startup. Every renewal failure cancels admission/apply immediately.
type fleetLease struct {
	r  Runner
	p  Plan
	mu sync.Mutex
}

var errMinerTips = errors.New("paused miners disagree on Core tip")

// Held miners keep following the public chain, and they are asked one after
// another: a block that arrives between two answers is not a disagreement.
// Ask again before giving up a lease that still has minutes to run.
func (l *fleetLease) check(ctx context.Context, renew bool) (int64, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if renew {
		// The renewal's time starts here, not while it waited for the lock
		// behind an admission check: that wait is not a lost lease.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
	}
	for attempt := 1; ; attempt++ {
		tip, hash, err := l.sweep(ctx, renew)
		if !errors.Is(err, errMinerTips) || attempt == 3 {
			return tip, hash, err
		}
	}
}
func (l *fleetLease) sweep(ctx context.Context, renew bool) (int64, string, error) {
	var tip, expiry int64
	var hash string
	for _, t := range miningTargets(l.p) {
		action := "mining_status"
		if renew {
			action = "mining_renew"
		}
		o, e := l.r.miningCall(ctx, l.p, t, action)
		if e != nil {
			return 0, "", e
		}
		original := l.p.Snapshot.Nodes[t.Name]
		if !SameIdentity(original, o, t) || Preserved(original, o, nil) != nil || o.Healthy(t) != nil {
			return 0, "", errors.New("managed miner identity or workload changed")
		}
		m := o.Mining
		baseline := l.p.Snapshot.Nodes[t.Name].Mining
		if m == nil || baseline == nil || !m.Paused || m.Active || m.OperationID != l.p.ID || m.ServiceHash != baseline.ServiceHash || m.ExpiresAt < time.Now().Add(2*time.Minute).Unix() {
			return 0, "", errors.New("fleet mining lease lost; no further withdrawal")
		}
		if o.Chain.CoreHeight < 1 || !digestRE.MatchString(o.Chain.CoreHash) {
			return 0, "", errors.New("paused Core checkpoint missing")
		}
		if tip != 0 && (o.Chain.CoreHeight != tip || o.Chain.CoreHash != hash) {
			return 0, "", errMinerTips
		}
		tip, hash = o.Chain.CoreHeight, o.Chain.CoreHash
		if expiry == 0 || m.ExpiresAt < expiry {
			expiry = m.ExpiresAt
		}
	}
	if tip < 1 || !digestRE.MatchString(hash) {
		return 0, "", errors.New("paused Core checkpoint missing")
	}
	// Each lease was checked when its miner answered. A slow later answer
	// must not outlast an earlier one: all of them hold at the same moment.
	if expiry < time.Now().Add(2*time.Minute).Unix() {
		return 0, "", errors.New("fleet mining lease lost; no further withdrawal")
	}
	return tip, hash, nil
}

func (l *fleetLease) keep(ctx context.Context, interval time.Duration) (context.Context, func() error) {
	run, cancel := context.WithCancelCause(ctx)
	done, stopped := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-run.Done():
				return
			case <-ticker.C:
				_, _, err := l.check(run, true)
				if err != nil {
					cancel(fmt.Errorf("fleet mining renewal: %w", err))
					return
				}
			}
		}
	}()
	return run, func() error {
		once.Do(func() { close(done) })
		<-stopped
		err := context.Cause(run)
		cancel(context.Canceled)
		return err
	}
}

func fleetRequest(p Plan, t Target, action string) Request {
	q := request(p.Snapshot.Fleet, t, action)
	before := p.Snapshot.Nodes[t.Name]
	q.Expected, q.Pins = &before, maps.Clone(p.Images[t.Name])
	q.Operation, q.OperationID, q.Migration = p.Operation, p.ID, p.Migration
	return q
}

func admissionHold(err error) bool {
	var e *workerError
	return errors.As(err, &e) && (e.code == "migration-epoch-window" || e.code == "migration-dkg-window" || e.code == "migration-dkg-active")
}

// The chain can advance through external miners. Check the checkpoint and DKG
// again on the worker immediately before it stops any service.
func (r Runner) fleetAdmission(ctx context.Context, p Plan, t Target, lease *fleetLease) (Request, error) {
	reported := false
	for {
		if err := VerifyCloud(ctx, p.Snapshot.Fleet, r.Identity, r.Cloud); err != nil {
			return Request{}, err
		}
		q := fleetRequest(p, t, "admit")
		if lease != nil {
			tip, hash, err := lease.check(ctx, false)
			if err != nil {
				return q, err
			}
			q.CoreCheckpointHeight, q.CoreCheckpointHash = tip, hash
		}
		h, err := r.withdrawalHealth(ctx, p)
		if err != nil {
			return q, err
		}
		if !healthSelected(h, p) {
			return q, errors.New("fleet unhealthy before withdrawal")
		}
		if err = canWithdrawSelected(h.Snapshot, t, p.Images[t.Name]); err != nil {
			return q, err
		}
		_, err = r.Remote.Call(ctx, q)
		if err == nil {
			q.Action = "apply"
			return q, nil
		}
		if !admissionHold(err) {
			return q, err
		}
		if !reported {
			r.report("epoch/DKG admission hold for " + t.Name + "; mining state unchanged")
			reported = true
		}
		if err = r.wait(ctx, readinessPoll); err != nil {
			return q, err
		}
	}
}

func fleetTargets(p Plan, current string) []Target {
	var out []Target
	for _, name := range append([]string{current}, p.Migration.Canaries...) {
		for _, t := range p.Snapshot.Fleet.Targets {
			if t.Name == name && p.Selects(t) && !slices.ContainsFunc(out, func(v Target) bool { return v.Name == t.Name }) {
				out = append(out, t)
			}
		}
	}
	for _, t := range p.Snapshot.Fleet.Targets {
		if p.Selects(t) && !slices.ContainsFunc(out, func(v Target) bool { return v.Name == t.Name }) {
			out = append(out, t)
		}
	}
	return out
}

// One plan, one successful mining pause, all validators. An error restores
// mining and leaves the exact validator journal resumable; it never skips to
// another node. Controller loss is covered by host-local independent timers.
func (r Runner) executeFleet(ctx context.Context, p Plan, record *Record, save func() error) (err error) {
	operationCtx := ctx
	targets := fleetTargets(p, record.Current)
	var pending []Target
	for _, t := range targets {
		if !record.Completed[t.Name] {
			pending = append(pending, t)
		}
	}
	// A saved final resume is idempotently reconciled without another pause.
	if len(pending) == 0 && (record.MiningPhase == "resuming" || record.MiningPhase == "resumed") {
		if err = r.resumeMining(p); err != nil {
			return
		}
		record.MiningPhase = "resumed"
		if err = save(); err != nil {
			return
		}
		if err = r.verify(ctx, p, *record); err != nil {
			return
		}
		record.Phase, record.LastError = "complete", ""
		return save()
	}
	// Once pause intent is journaled, even a lost response requires cleanup.
	owned := record.MiningPhase == "pausing" || record.MiningPhase == "paused"
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
	// Recover a withdrawn validator first if an earlier failed run already
	// restored mining. Do not wait for a new admission window to restore quorum.
	if !owned && record.Current != "" && !record.Completed[record.Current] {
		t := targets[0]
		o, e := r.Remote.Call(ctx, request(p.Snapshot.Fleet, t, "observe"))
		if e != nil || o.Healthy(t) != nil {
			q := fleetRequest(p, t, "apply")
			if _, err = r.Remote.Call(ctx, q); err != nil {
				return
			}
			if err = r.awaitReady(ctx, p.Snapshot.Fleet, t); err != nil {
				return
			}
			if err = r.verify(ctx, p, *record); err != nil {
				return
			}
			record.Completed[t.Name], record.Current = true, ""
			if err = save(); err != nil {
				return
			}
			pending = pending[1:]
		}
	}
	if !owned && len(pending) > 0 {
		// Do all staging and initial progress/admission checks before pausing.
		if _, err = r.fleetAdmission(ctx, p, pending[0], nil); err != nil {
			return
		}
	}
	if !owned && len(pending) == 0 {
		if err = r.verify(ctx, p, *record); err != nil {
			return
		}
		record.Phase, record.LastError = "complete", ""
		return save()
	}
	if !owned {
		record.MiningPhase = "pausing"
		if err = save(); err != nil {
			return
		}
		owned = true
	}
	for _, t := range miningTargets(p) {
		// Idempotent for a still-owned pause; never silently reacquire a lease
		// after its watchdog resumed mining during a previously paused fleet.
		if record.MiningPhase == "paused" {
			o, e := r.miningCall(ctx, p, t, "mining_status")
			if e != nil {
				return e
			}
			if o.Mining == nil || !o.Mining.Paused || o.Mining.ExpiresAt <= time.Now().Unix() {
				return errors.New("fleet pause expired; reconcile before retry")
			}
		}
		if _, err = r.miningCall(ctx, p, t, "mining_pause"); err != nil {
			return
		}
	}
	lease := &fleetLease{r: r, p: p}
	if _, _, err = lease.check(ctx, true); err != nil {
		return
	}
	ctx, stop = lease.keep(ctx, time.Minute)
	record.MiningPhase = "paused"
	if err = save(); err != nil {
		return
	}
	r.fleetPaused = true
	r.report("owned miners paused for the complete fleet operation")
	for _, t := range pending {
		fresh := record.Current == ""
		if !fresh {
			o, e := r.Remote.Call(ctx, request(p.Snapshot.Fleet, t, "observe"))
			fresh = e == nil && o.Healthy(t) == nil
		}
		q := fleetRequest(p, t, "apply")
		if fresh {
			if q, err = r.fleetAdmission(ctx, p, t, lease); err != nil {
				return
			}
		} else {
			if q.CoreCheckpointHeight, q.CoreCheckpointHash, err = lease.check(ctx, false); err != nil {
				return
			}
		}
		record.Current, record.Phase = t.Name, "applying"
		if err = save(); err != nil {
			return
		}
		r.report("managed target " + t.Name + " applying; fleet mining remains paused")
		run, cancel := context.WithTimeout(ctx, 10*time.Minute)
		_, err = r.Remote.Call(run, q)
		if err == nil {
			err = r.awaitReady(run, p.Snapshot.Fleet, t)
		}
		cancel()
		// A safe admission race may close the window after preflight but before
		// any service stops. Leave its journal pending; no withdrawal is skipped.
		if admissionHold(err) && fresh {
			// A worker refusal is before withdrawal. Wait inside the same pause and
			// recheck health/quorum/epoch/DKG, then replay the same journaled target.
			for admissionHold(err) {
				if q, err = r.fleetAdmission(ctx, p, t, lease); err != nil {
					return
				}
				run, cancel := context.WithTimeout(ctx, 10*time.Minute)
				_, err = r.Remote.Call(run, q)
				if err == nil {
					err = r.awaitReady(run, p.Snapshot.Fleet, t)
				}
				cancel()
			}
		}
		if err != nil {
			return
		}
		check := request(p.Snapshot.Fleet, t, "observe")
		check.CoreCheckpointHeight = q.CoreCheckpointHeight
		o, e := r.Remote.Call(ctx, check)
		if e != nil {
			return e
		}
		if o.Chain.CoreCheckpointHash != q.CoreCheckpointHash || o.Chain.CoreHeight < q.CoreCheckpointHeight {
			return errors.New("restarted node lost paused Core checkpoint")
		}
		if _, _, err = lease.check(ctx, false); err != nil {
			return
		}
		record.Phase = "verifying"
		if err = save(); err != nil {
			return
		}
		if err = r.verify(ctx, p, *record); err != nil {
			return
		}
		record.Completed[t.Name], record.Current = true, ""
		if err = save(); err != nil {
			return
		}
		count := 0
		for _, target := range targets {
			if record.Completed[target.Name] {
				count++
			}
		}
		if count == 2 {
			r.report("both architecture canaries verified; miners remain paused")
		} else if count > 2 && (count-2)%3 == 0 {
			r.report(fmt.Sprintf("review batch verified: %d/%d validators; miners remain paused", count, len(targets)))
		}
	}
	if err = r.verify(ctx, p, *record); err != nil {
		return
	}
	// Final common checkpoint is required across the entire upgraded fleet.
	tip, hash, e := lease.check(ctx, false)
	if e != nil {
		return e
	}
	for _, t := range targets {
		q := request(p.Snapshot.Fleet, t, "observe")
		q.CoreCheckpointHeight = tip
		o, e := r.Remote.Call(ctx, q)
		if e != nil {
			return e
		}
		if o.Chain.CoreHeight < tip || o.Chain.CoreCheckpointHash != hash {
			return errors.New("fleet Core checkpoint not verified")
		}
	}
	if err = stop(); err != nil {
		stop = nil
		return
	}
	stop = nil
	record.MiningPhase = "resuming"
	if err = save(); err != nil {
		return
	}
	r.report("all selected validators verified; resuming owned miners once")
	if err = r.resumeMining(p); err != nil {
		return
	}
	owned = false
	record.MiningPhase = "resumed"
	if err = save(); err != nil {
		return
	}
	r.fleetPaused = false
	ctx = operationCtx
	// Only now require new Core and ChainLock blocks as well as Platform.
	if err = r.verify(ctx, p, *record); err != nil {
		return
	}
	record.Phase, record.LastError = "complete", ""
	r.report("complete fleet verified with post-resumption Core/ChainLock/Platform progress")
	return save()
}
