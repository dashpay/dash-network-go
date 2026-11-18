package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/rollout"
)

func (u *UpgradePlan) WithRollout(options rollout.Options) error {
	if options.PauseMining {
		options.Miners = []string{u.Deployment.Miner().Name}
	}
	if options.PauseMining || u.Scope == "core" {
		options.MiningRecipe = node.MiningDigest()
	}
	u.Rollout = &options
	u.ID = ""
	u.ID = hash(*u)
	return u.Validate()
}
func (u UpgradePlan) validateRollout() error {
	if u.Rollout == nil {
		return nil
	}
	if err := u.Rollout.Validate(); err != nil {
		return err
	}
	if u.Rollout.PauseMining && !slices.Equal(u.Rollout.Miners, []string{u.Deployment.Miner().Name}) {
		return errors.New("pause must name the deployment's exact owned miner")
	}
	if (u.Rollout.PauseMining || u.Scope == "core") && u.Rollout.MiningRecipe != node.MiningDigest() {
		return errors.New("mining control recipe changed; replan with exact executor")
	}
	return nil
}

// A host refuses to report a service whose running image is not the one the
// journal intends for it. For a journaled batch member that is the signature
// of a node the rollout has not reached: still serving on its previous images.
func untouched(s samples) bool {
	for _, problem := range s.problems {
		if !strings.Contains(problem, "node running-image-drift") {
			return false
		}
	}
	return len(s.problems) > 0
}
func serving(t node.Target, s samples) bool {
	return len(s.problems) == 0 && s.core != nil && s.core.Synced && (t.Role != "validator" || (s.core.MasternodeState == "READY" && s.platform != nil && !s.platform.CatchingUp))
}

// resumeAdmission protects a journaled batch that was never withdrawn. The
// journal records intent only, so resuming would take every untouched member
// down now, in a fleet that may have degraded since the batch was admitted.
// A member that is down or mid-replacement always takes precedence: the batch
// is then reconciled exactly as journaled.
func (e *execution) resumeAdmission(targets []node.Target, observed map[string]samples) error {
	var fresh []node.Target
	for _, t := range targets {
		s := observed[t.Name]
		if untouched(s) {
			fresh = append(fresh, t)
		} else if !serving(t, s) {
			return nil
		}
	}
	if err := e.admission(fresh, observed, true); err != nil {
		return fmt.Errorf("journaled batch not resumed: %w", err)
	}
	return nil
}

func (e *execution) batchAdmission(targets []node.Target, observed map[string]samples) error {
	return e.admission(targets, observed, false)
}

// On a resume the batch members themselves cannot report the validator set.
func (e *execution) admission(targets []node.Target, observed map[string]samples, resuming bool) error {
	excluded := map[string]bool{}
	for _, t := range targets {
		if t.Role == "validator" {
			excluded[strings.ToLower(e.r.Deployment.Nodes[t.Name].ProTxHash)] = true
		}
	}
	if len(excluded) == 0 {
		return nil
	}
	healthy := map[string]bool{}
	for _, t := range e.p.Validators() {
		s := observed[t.Name]
		if s.core != nil && s.core.Synced && s.core.MasternodeState == "READY" && s.platform != nil && !s.platform.CatchingUp && s.platform.Height > 0 {
			healthy[strings.ToLower(e.r.Deployment.Nodes[t.Name].ProTxHash)] = true
		}
	}
	for _, t := range e.p.Validators() {
		s := observed[t.Name]
		if resuming && excluded[strings.ToLower(e.r.Deployment.Nodes[t.Name].ProTxHash)] {
			continue
		}
		if s.platform == nil || len(s.platform.Validators) < platformQuorumMinimum {
			return errors.New("batch quorum membership unavailable")
		}
		remaining := 0
		for _, id := range s.platform.Validators {
			if healthy[strings.ToLower(id)] && !excluded[strings.ToLower(id)] {
				remaining++
			}
		}
		if remaining <= 2*len(s.platform.Validators)/3 {
			return fmt.Errorf("requested batch lacks observed >2/3 remaining voting power at %s; reduce concurrency", t.Name)
		}
	}
	return nil
}

// miningHeld reports a journal that may still own a pause on the miner host.
func miningHeld(phase string) bool {
	return phase == "pausing" || phase == "paused" || phase == "resuming"
}

func (e *execution) miningRequest(u UpgradePlan, action string) node.Request {
	q := runtimeRequest(e.p, e.r, e.p.Miner(), action)
	q.MiningControl = &node.MiningControl{OperationID: u.ID, ContainerID: e.r.Upgrade.MiningContainerID, RecipeSHA256: u.Rollout.MiningRecipe}
	return q
}

// The miner host's lock is non-blocking and every observation of that host
// takes it too, so a mining control call can meet a health sample, and the
// renewer can meet either. Nothing ran on a busy host: ask again shortly. A
// host that stays busy, and every other failure, is returned as before.
var hostBusyRetry = 3 * time.Second

const hostBusyAttempts = 6

func hostBusy(err error) bool {
	s := node.SafeError(err)
	return strings.Contains(s, "node host-busy") || strings.Contains(s, "node blockingioerror")
}
func (e *execution) miningCall(ctx context.Context, q node.Request) (o node.Observation, err error) {
	for attempt := 1; ; attempt++ {
		o, err = e.runner.Remote.Call(ctx, q)
		if err == nil || !hostBusy(err) || attempt == hostBusyAttempts {
			return o, err
		}
		timer := time.NewTimer(hostBusyRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return o, err
		case <-timer.C:
		}
	}
}
func (e *execution) checkMining(u UpgradePlan, action string) error {
	o, err := e.miningCall(e.ctx, e.miningRequest(u, action))
	if err != nil {
		return err
	}
	if o.MiningPause == nil || !o.MiningPause.Paused || o.MiningPause.OperationID != u.ID || o.MiningPause.ContainerID != e.r.Upgrade.MiningContainerID || o.MiningPause.ExpiresAt < time.Now().Add(2*time.Minute).Unix() {
		return errors.New("owned mining lease lost; stop rollout")
	}
	return nil
}
func (e *execution) pauseRolloutMining(u UpgradePlan) error {
	o, err := e.call(e.p.Miner(), "core-status", nil)
	if err != nil {
		return err
	}
	if o.Core == nil || o.Core.Mining == nil || len(o.Core.Mining.ContainerID) != 64 {
		return errors.New("owned mining baseline unavailable")
	}
	e.r.Upgrade.MiningContainerID = o.Core.Mining.ContainerID
	e.r.Upgrade.MiningPhase = "pausing"
	if err = e.save(); err != nil {
		return err
	}
	deadline := time.Now().Add(e.cycleWait())
	for {
		if u.Scope == "core" && len(e.r.Upgrade.PendingNodes) == 0 {
			o, err = e.call(e.p.Miner(), "core-status", nil)
			if err != nil {
				return err
			}
			if o.Core == nil || !quietDKG(o.Core.Height) {
				if time.Now().After(deadline) {
					return errors.New("no quiet DKG window before batch")
				}
				if err = e.pause(2 * time.Second); err != nil {
					return err
				}
				continue
			}
		}
		if err = e.checkMining(u, "rollout-mining-pause"); err != nil {
			return err
		}
		if u.Scope != "core" || len(e.r.Upgrade.PendingNodes) > 0 {
			break
		}
		o, err = e.call(e.p.Miner(), "core-status", nil)
		if err == nil && o.Core != nil && quietDKG(o.Core.Height) {
			break
		}
		if err = e.resumeRolloutMining(u); err != nil {
			return err
		}
	}
	e.runner.miningPaused = true
	e.r.Upgrade.MiningPhase = "paused"
	e.report("owned mining paused for the reviewed upgrade")
	return e.save()
}
func (e *execution) resumeRolloutMining(u UpgradePlan) error {
	e.r.Upgrade.MiningPhase = "resuming"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := e.miningCall(ctx, e.miningRequest(u, "rollout-mining-resume"))
	if err == nil && (o.MiningPause == nil || o.MiningPause.Paused || o.MiningPause.ContainerID != e.r.Upgrade.MiningContainerID || o.MiningPause.OperationID != u.ID) {
		err = errors.New("mining resume unverified")
	}
	if err == nil {
		e.runner.miningPaused = false
		e.r.Upgrade.MiningPhase = "resumed"
		e.report("mining resumed")
	}
	return err
}

func (e *execution) upgradeBatches(u UpgradePlan) (health Health, err error) {
	originalCtx := e.ctx
	var stop func() error
	owned := false
	cleanup := func() error {
		var result error
		if stop != nil {
			result = stop()
			stop = nil
		}
		e.ctx = originalCtx
		if owned {
			result = errors.Join(result, e.resumeRolloutMining(u))
			if !e.runner.miningPaused {
				owned = false
			}
		}
		return result
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	// A pause journaled by an interrupted run is this operation's to restore.
	// begin adopts it while a batch still needs blocks held. With only the
	// miner (or nothing) left, nothing below would resume it: blocks would
	// return only with the host's own watchdog, after the final health gate
	// had already waited for them.
	if phase := e.r.Upgrade.MiningPhase; miningHeld(phase) {
		held := false
		for _, t := range upgradeOrder(e.p, u.Scope) {
			held = held || (!e.r.Upgrade.Completed[t.Name] && t.Name != e.p.Miner().Name)
		}
		if !held {
			if err = e.resumeRolloutMining(u); err != nil {
				// A pause that was journaled but never reached the host left no lease there.
				if phase != "pausing" || !strings.Contains(node.SafeError(err), "node mining-owner-or-drift") {
					return
				}
				err = nil
				e.r.Upgrade.MiningPhase = "resumed"
			}
			if err = e.save(); err != nil {
				return
			}
		}
	}
	begin := func() error {
		owned = true
		if x := e.pauseRolloutMining(u); x != nil {
			return x
		}
		// Capture immutable request intent before workers or journal checkpoints.
		request := e.miningRequest(u, "rollout-mining-renew")
		e.ctx, stop = rollout.Keep(originalCtx, func(ctx context.Context) error {
			o, x := e.miningCall(ctx, request)
			if x == nil && (o.MiningPause == nil || !o.MiningPause.Paused || o.MiningPause.OperationID != u.ID || o.MiningPause.ExpiresAt < time.Now().Add(2*time.Minute).Unix()) {
				x = errors.New("mining lease renewal lost")
			}
			return x
		})
		return nil
	}
	for {
		var targets []node.Target
		if len(e.r.Upgrade.PendingNodes) > 0 {
			for _, name := range e.r.Upgrade.PendingNodes {
				for _, t := range upgradeOrder(e.p, u.Scope) {
					if t.Name == name {
						targets = append(targets, t)
					}
				}
			}
		} else {
			for _, t := range upgradeOrder(e.p, u.Scope) {
				if e.r.Upgrade.Completed[t.Name] {
					continue
				}
				if t.Name == e.p.Miner().Name && len(targets) > 0 {
					break
				}
				targets = append(targets, t)
				if len(targets) == u.Rollout.Concurrency || t.Name == e.p.Miner().Name {
					break
				}
			}
		}
		if len(targets) == 0 {
			break
		}
		minerBatch := targets[0].Name == e.p.Miner().Name
		// The miner is upgraded last, alone, after generation is restored. Its own
		// Core upgrade retains the existing graceful stop/restart semantics.
		if minerBatch && owned {
			if err = cleanup(); err != nil {
				return
			}
			if health, err = e.verifyUpgrade(); err != nil {
				return
			}
		}
		if !minerBatch && !owned && (u.Rollout.PauseMining || u.Scope == "core") {
			if err = begin(); err != nil {
				return
			}
		}
		if owned {
			if err = e.checkMining(u, "rollout-mining-status"); err != nil {
				return
			}
		}
		if len(e.r.Upgrade.PendingNodes) > 0 {
			if err = e.resumeAdmission(targets, e.runner.sample(e.ctx, e.p, e.r, 0)); err != nil {
				return
			}
		}
		if len(e.r.Upgrade.PendingNodes) == 0 {
			if err = liveScope(e.ctx, e.p, e.r, e.runner.Cloud); err != nil {
				return
			}
			if health, err = e.verifyUpgrade(); err != nil {
				return
			}
			observed := e.runner.sample(e.ctx, e.p, e.r, 0)
			for {
				err = e.batchAdmission(targets, observed)
				if err == nil {
					break
				}
				if len(targets) == 1 {
					return
				}
				targets = targets[:len(targets)-1]
			}
			for _, t := range targets {
				e.r.Upgrade.PendingNodes = append(e.r.Upgrade.PendingNodes, t.Name)
				e.r.Runtime.Images[t.Name] = cloneImages(u.To)[t.Name]
			}
			e.r.Upgrade.Phase = "applying"
			if err = e.save(); err != nil {
				return
			}
		}
		type result struct {
			name        string
			observation node.Observation
			err         error
		}
		results := make(chan result, len(targets))
		var wg sync.WaitGroup
		// Prepare every request on the journal-owning goroutine.
		for _, t := range targets {
			q := e.upgradeRequest(u, t, "upgrade-apply")
			e.report("upgrade " + t.Name + " applying")
			wg.Add(1)
			go func(name string, q node.Request) {
				defer wg.Done()
				o, x := e.runner.Remote.Call(e.ctx, q)
				results <- result{name, o, x}
			}(t.Name, q)
		}
		wg.Wait()
		close(results)
		for x := range results {
			if x.err != nil {
				err = errors.Join(err, fmt.Errorf("%s: %w", x.name, x.err))
				continue
			}
			b := e.r.Upgrade.Baseline[x.name]
			if u.Scope == "core" {
				if x.observation.Core == nil || x.observation.Core.ConfigSHA256 != b.CoreConfig || x.observation.Core.Genesis != b.CoreGenesis {
					err = errors.Join(err, fmt.Errorf("%s: Core preservation failed", x.name))
				}
			} else if !preservedCore(x.observation.Core, b) || x.observation.Render == nil || !slices.Equal(x.observation.Render.Changes, e.r.Upgrade.Changes[x.name]) {
				err = errors.Join(err, fmt.Errorf("%s: staged changes or Core preservation failed", x.name))
			}
		}
		if err != nil {
			return
		}
		// As on the serial path: blocks stay held until every replaced
		// validator Core is READY and reconnected, so no DKG session can
		// start without it (PoSe). The health gate alone asks for one peer.
		if u.Scope == "core" {
			if err = e.settle(targets...); err != nil {
				return
			}
		}
		if owned {
			if err = e.checkMining(u, "rollout-mining-status"); err != nil {
				return
			}
		}
		e.r.Upgrade.Phase = "verifying"
		e.report("upgrade-verifying")
		if err = e.save(); err != nil {
			return
		}
		if health, err = e.verifyUpgrade(); err != nil {
			return
		}
		for _, t := range targets {
			e.r.Upgrade.Completed[t.Name] = true
		}
		e.r.Upgrade.PendingNodes = nil
		if err = e.save(); err != nil {
			return
		}
		for _, t := range targets {
			e.report("upgrade " + t.Name + " verified; whole fleet healthy")
		}
		if owned && !u.Rollout.PauseMining {
			if err = cleanup(); err != nil {
				return
			}
			if health, err = e.verifyUpgrade(); err != nil {
				return
			}
		}
	}
	if err = cleanup(); err != nil {
		return
	}
	return e.verifyUpgrade()
}
