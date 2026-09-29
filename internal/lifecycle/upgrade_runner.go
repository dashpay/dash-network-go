package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
)

// componentServices names the dashmate service of each Platform component.
var componentServices = map[string]string{"drive": "drive_abci", "tenderdash": "drive_tenderdash", "dapi": "rs_dapi", "gateway": "gateway"}

// platformQuorumMinimum is llmq_devnet_platform's minimum size (12/9/8).
const platformQuorumMinimum = 9

func preservedCore(c *node.Core, b provision.Preservation) bool {
	return c != nil && c.ContainerID == b.CoreID && c.StartedAt == b.CoreStarted && c.ConfigSHA256 == b.CoreConfig && c.Genesis == b.CoreGenesis
}

func missingUpgradeObservation(message, target string, problems []string) error {
	if len(problems) > 0 {
		// sample() already sanitizes remote failures. Retain those reasons so
		// an operator can distinguish a failed RPC from a failed SSH connection.
		return fmt.Errorf("%s at %s: %s", message, target, strings.Join(problems, "; "))
	}
	return fmt.Errorf("%s at %s", message, target)
}

// upgradeOrder is the withdrawal order: validators one at a time; for a Core
// upgrade then fullnodes and other nodes, and the mining node last.
func upgradeOrder(p Plan, scope string) []node.Target {
	if scope != "core" {
		return p.Validators()
	}
	miner := p.Miner().Name
	out := p.Validators()
	for _, role := range []string{"fullnode", "wallet", "miner"} {
		for _, t := range p.Targets {
			if t.Role == role && t.Name != miner {
				out = append(out, t)
			}
		}
	}
	return append(out, p.Miner())
}

func upgradeEvidence(p Plan, record provision.Record, observed map[string]samples, pending string) (map[string]provision.Preservation, error) {
	baseline := map[string]provision.Preservation{}
	coreScope := record.Upgrade != nil && record.Upgrade.Scope == "core"
	known := map[string]bool{}
	for _, t := range p.Validators() {
		known[record.Deployment.Nodes[t.Name].ProTxHash] = true
	}
	for _, t := range p.Targets {
		s := observed[t.Name]
		if coreScope && t.Name == pending && s.core == nil {
			// The node whose Core is being replaced may be on either image, or
			// briefly down; its resumed apply reconciles it from the host marker.
			baseline[t.Name] = record.Upgrade.Baseline[t.Name]
			continue
		}
		if s.core == nil || s.core.Genesis != record.Deployment.CoreGenesis {
			return nil, missingUpgradeObservation("missing/mismatched Core", t.Name, s.problems)
		}
		if _, err := time.Parse(time.RFC3339Nano, s.core.StartedAt); err != nil {
			return nil, fmt.Errorf("missing Core process start evidence at %s", t.Name)
		}
		b := provision.Preservation{CoreID: s.core.ContainerID, CoreStarted: s.core.StartedAt, CoreConfig: s.core.ConfigSHA256, CoreGenesis: s.core.Genesis}
		// A node whose Core this upgrade replaced keeps its configuration and
		// genesis; every other node keeps the exact Core process.
		touched := coreScope && (record.Upgrade.Completed[t.Name] || t.Name == record.Upgrade.CurrentNode)
		if touched {
			old := record.Upgrade.Baseline[t.Name]
			if s.core.ConfigSHA256 != old.CoreConfig || s.core.Genesis != old.CoreGenesis {
				return nil, fmt.Errorf("Core configuration or genesis changed at %s", t.Name)
			}
		} else if record.Upgrade != nil && !preservedCore(s.core, record.Upgrade.Baseline[t.Name]) {
			return nil, fmt.Errorf("preserved Core changed at %s", t.Name)
		}
		if t.Role == "validator" {
			x := s.platform
			if x == nil {
				if t.Name == pending {
					baseline[t.Name] = b
					continue
				}
				return nil, missingUpgradeObservation("Platform observation missing", t.Name, s.problems)
			}
			// Platform quorums are 12 members; one that formed with fewer (at
			// least the minimum of 9, e.g. after a member missed a DKG) is live.
			if x.Protocol != p.InitialProtocolVersion || len(x.Validators) < platformQuorumMinimum || len(x.Validators) > 12 {
				return nil, fmt.Errorf("unsupported live protocol or quorum size at %s", t.Name)
			}
			seen := map[string]bool{}
			for _, id := range x.Validators {
				id = strings.ToLower(id)
				if seen[id] || !known[id] {
					return nil, fmt.Errorf("unknown/duplicate live quorum member at %s", t.Name)
				}
				seen[id] = true
			}
			b.Containers = x.Containers
			b.Restarts = x.Restarts
			if record.Upgrade != nil {
				old := record.Upgrade.Baseline[t.Name]
				for _, component := range []string{"drive", "tenderdash", "dapi", "gateway"} {
					if coreScope {
						// Platform containers survive a Core upgrade; the node being
						// upgraded restarts them gracefully, which resets counters.
						if x.Containers[component] != old.Containers[component] || (!touched && x.Restarts[component] != old.Restarts[component]) {
							return nil, fmt.Errorf("Platform service changed during Core upgrade: %s/%s", t.Name, component)
						}
						continue
					}
					// A node the rollout has reached runs its target images and
					// every service the target release reconfigured anew.
					reached := record.Upgrade.Completed[t.Name] || t.Name == record.Upgrade.CurrentNode
					changes := record.Upgrade.Changes[t.Name]
					if reached && slices.Contains(changes, componentServices[component]) {
						continue
					}
					expectedRestarts := old.Restarts[component]
					if component == "tenderdash" && reached && slices.Contains(changes, "drive_abci") {
						// A planned graceful stop/start around Drive preserves the
						// Tenderdash container/image, but resets Docker's counter.
						expectedRestarts = 0
					}
					if record.Runtime.Images[t.Name][component] == record.Upgrade.From[t.Name][component] && (x.Containers[component] != old.Containers[component] || x.Restarts[component] != expectedRestarts) {
						return nil, fmt.Errorf("unselected/not-yet-upgraded service changed: %s/%s", t.Name, component)
					}
				}
			}
		}
		baseline[t.Name] = b
	}
	return baseline, nil
}

func (e *execution) upgradeRequest(u UpgradePlan, t node.Target, action string) node.Request {
	q := runtimeRequest(e.p, e.r, t, action)
	q.Upgrade = &node.ImageChange{ID: u.ID, PreviousID: u.PreviousID, From: u.From[t.Name], To: u.To[t.Name], Preserve: e.r.Upgrade.Baseline[t.Name]}
	if u.Scope == "core" {
		q.Upgrade.Scope = "core"
	}
	q.Context.SidecarImages = nil
	if pins := upgradeSidecars(e.r).For(t.Architecture); len(pins) > 0 {
		q.Context.SidecarImages = pins
	}
	return q
}

// upgradeSidecars are the sidecar pins a rollout installs: the target
// release's when they differ, else the fleet's.
func upgradeSidecars(record provision.Record) provision.Sidecars {
	if record.Upgrade != nil && len(record.Upgrade.Sidecars) > 0 {
		return record.Upgrade.Sidecars
	}
	return effectiveSidecars(record)
}

func (e *execution) stageUpgrade(u UpgradePlan) (map[string]*node.Render, error) {
	// Cache images and render the target release concurrently, but never
	// withdraw a service in this phase.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	observed := map[string]*node.Render{}
	limit := make(chan struct{}, 4)
	for _, t := range upgradeOrder(e.p, u.Scope) {
		wg.Add(1)
		go func(t node.Target) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-e.ctx.Done():
				mu.Lock()
				failures = append(failures, t.Name+": staging cancelled")
				mu.Unlock()
				return
			}
			defer func() { <-limit }()
			o, err := e.runner.Remote.Call(e.ctx, e.upgradeRequest(u, t, "upgrade-stage"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, t.Name+": "+node.SafeError(err))
			} else if o.Render != nil {
				observed[t.Name] = o.Render
			}
		}(t)
	}
	wg.Wait()
	if len(failures) > 0 {
		sort.Strings(failures)
		return nil, errors.New("image staging failed: " + strings.Join(failures, "; "))
	}
	return observed, nil
}

// stagePlatform stages a Platform rollout: every validator renders the target
// release. Sidecar images that release requests beyond the pinned ones are
// pinned and staged again; each validator's staged changes are journaled once.
func (e *execution) stagePlatform(u UpgradePlan) error {
	for pass := 0; ; pass++ {
		observed, err := e.stageUpgrade(u)
		if err != nil {
			return err
		}
		validators := map[string]*node.Render{}
		for _, t := range e.p.Validators() {
			validators[t.Name] = observed[t.Name]
		}
		version, requested, err := renders(validators)
		if err != nil {
			return err
		}
		current := upgradeSidecars(e.r)
		pins, err := e.runner.pinSidecars(e.ctx, e.p, current, requested)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(pins, current) {
			if pass > 0 {
				return errors.New("sidecar images changed while staging")
			}
			e.r.Upgrade.Sidecars = pins
			if err = e.save(); err != nil {
				return err
			}
			e.report(fmt.Sprintf("dashmate %s requests new sidecar images; pinned %d and staging again", version, len(pins)))
			continue
		}
		if e.r.Upgrade.Changes == nil {
			e.r.Upgrade.Changes = map[string][]string{}
		}
		deferred := false
		for _, t := range e.p.Validators() {
			r := observed[t.Name]
			if _, ok := e.r.Upgrade.Changes[t.Name]; !ok && !e.r.Upgrade.Completed[t.Name] && t.Name != e.r.Upgrade.CurrentNode {
				e.r.Upgrade.Changes[t.Name] = append([]string{}, r.Changes...)
			}
			deferred = deferred || len(r.Deferred) > 0
		}
		if err = e.save(); err != nil {
			return err
		}
		e.report("dashmate " + version + " renders every validator for this rollout")
		if deferred {
			e.report("the target release also changes Core's rendered configuration; that takes effect at the next Core rollout")
		}
		return nil
	}
}

func (e *execution) verifyUpgrade() (Health, error) {
	// A Doctor observation alone spans PlatformObservationWindow.
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Minute)
	defer cancel()
	for {
		h, err := e.runner.Doctor(ctx, e.p, e.r)
		if err != nil {
			return h, err
		}
		if h.Healthy {
			_, err = upgradeEvidence(e.p, e.r, e.runner.sample(ctx, e.p, e.r, 0), "")
			return h, err
		}
		e.report("upgrade health gate waiting: " + strings.Join(h.Problems, "; "))
		if err = ctx.Err(); err != nil {
			return h, err
		}
		if e.runner.Wait != nil {
			if err = e.runner.Wait(ctx); err != nil {
				return h, err
			}
		} else {
			timer := time.NewTimer(15 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return h, ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// Upgrade stages all images, then changes one validator at a time. The durable
// current-node intent is written before SSH; resume repairs only that node before
// allowing another withdrawal. No rollback is inferred from a failed gate.
func (r Runner) Upgrade(ctx context.Context, u UpgradePlan) (result provision.Record, err error) {
	if err = u.Validate(); err != nil {
		return
	}
	if r.Remote == nil || r.Owner == "" {
		return result, errors.New("authenticated backend and runner identity required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, errors.New("upgrade requires a deadline")
	}
	p := u.Deployment
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
			e.r.Upgrade.Phase = "interrupted"
			e.r.LastError = node.SafeError(err)
			e.r.UpdatedAt = time.Now().UTC()
			e.r.Revision++
			if saveErr := r.Store.Save(cleanup, e.r, r.Owner); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("save upgrade interruption: %w", saveErr))
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
	if e.r.Deployment == nil || e.r.Deployment.PlanID != p.ID || e.r.Deployment.Phase != "network-ready" {
		return result, errors.New("upgrade requires the original ready deployment")
	}
	if err = liveScope(ctx, p, e.r, r.Cloud); err != nil {
		return
	}
	resuming := e.r.Upgrade != nil && e.r.Upgrade.PlanID == u.ID
	if resuming {
		progress := e.r.Upgrade
		if progress.PreviousID != u.PreviousID || !reflect.DeepEqual(progress.From, u.From) || !reflect.DeepEqual(progress.To, u.To) {
			return result, errors.New("upgrade differs from saved intent")
		}
	} else {
		if e.r.Upgrade != nil && e.r.Upgrade.Phase != "complete" {
			return result, errors.New("a different unfinished upgrade owns this network")
		}
		previous := ""
		if e.r.Runtime != nil {
			previous = e.r.Runtime.UpgradeID
		}
		from, sourceErr := effectiveImages(p, e.r)
		if sourceErr != nil {
			return result, sourceErr
		}
		if previous != u.PreviousID || !reflect.DeepEqual(from, u.From) {
			return result, errors.New("upgrade source changed after planning")
		}
		h, healthErr := r.Doctor(ctx, p, e.r)
		if healthErr != nil {
			return result, healthErr
		}
		if !h.Healthy {
			return result, errors.New("upgrade requires every target healthy before any image change: " + strings.Join(h.Problems, "; "))
		}
		// A completed previous rollout is history, not this operation's baseline.
		baselineRecord := e.r
		baselineRecord.Upgrade = nil
		baseline, baselineErr := upgradeEvidence(p, baselineRecord, r.sample(ctx, p, e.r, 0), "")
		if baselineErr != nil {
			return result, baselineErr
		}
		e.r.Runtime = &provision.RuntimeState{DeploymentID: p.ID, UpgradeID: u.ID, Images: cloneImages(u.From), Sidecars: effectiveSidecars(e.r)}
		e.r.Upgrade = &provision.UpgradeProgress{PlanID: u.ID, PreviousID: u.PreviousID, Phase: "staging", From: cloneImages(u.From), To: cloneImages(u.To), Baseline: baseline, Completed: map[string]bool{}}
		if u.Scope == "core" {
			e.r.Upgrade.Scope = "core"
		}
		for _, t := range upgradeOrder(p, u.Scope) {
			e.r.Upgrade.Completed[t.Name] = false
		}
		changed = true
		e.r.LastRunner = r.Owner
		e.r.CLIVersion = r.Version
		e.r.LastError = ""
		if err = e.save(); err != nil {
			return
		}
	}
	if _, err = upgradeEvidence(p, e.r, r.sample(ctx, p, e.r, 0), e.r.Upgrade.CurrentNode); err != nil {
		return
	}
	if e.r.Upgrade.Phase == "complete" {
		_, err = e.verifyUpgrade()
		return
	}
	changed = true
	if u.Scope == "core" {
		e.report("stage the exact Core image on every node")
	} else {
		e.report("stage exact upgrade images on all validators")
	}
	e.report("upgrade-staging")
	if u.Scope == "core" {
		_, err = e.stageUpgrade(u)
	} else {
		err = e.stagePlatform(u)
	}
	if err != nil {
		return
	}
	var health Health
	if e.r.Upgrade.CurrentNode == "" {
		if health, err = e.verifyUpgrade(); err != nil {
			return
		}
	}
	// Recover the previously journaled withdrawal before scheduling any other.
	targets := upgradeOrder(p, u.Scope)
	if current := e.r.Upgrade.CurrentNode; current != "" {
		sort.SliceStable(targets, func(i, j int) bool { return targets[i].Name == current && targets[j].Name != current })
	}
	for _, t := range targets {
		if e.r.Upgrade.Completed[t.Name] {
			continue
		}
		if err = liveScope(ctx, p, e.r, r.Cloud); err != nil {
			return
		}
		e.r.Upgrade.Phase = "applying"
		e.r.Upgrade.CurrentNode = t.Name
		e.r.Runtime.Images[t.Name] = cloneImages(u.To)[t.Name]
		if err = e.save(); err != nil {
			return
		}
		if u.Scope == "core" {
			e.report("upgrade Core on " + t.Name + "; configuration, keys and data preserved")
		} else {
			e.report("upgrade " + t.Name + "; Core preserved")
		}
		e.report("upgrade-applying")
		// DKG sessions advance only with blocks, and this network's miner is
		// ours: hold block production while a validator's Core is replaced, so
		// no session can start before it is back and reconnected (PoSe).
		paused := false
		if u.Scope == "core" && t.Role == "validator" {
			if err = e.pauseMiningQuietly(); err != nil {
				return
			}
			paused = true
		}
		var observed node.Observation
		observed, err = r.Remote.Call(ctx, e.upgradeRequest(u, t, "upgrade-apply"))
		if paused {
			if err == nil {
				err = e.settle(t)
			}
			// Always resume, even after a failure or cancellation.
			if startErr := e.resumeMining(); startErr != nil {
				err = errors.Join(err, startErr)
			}
		}
		if err != nil {
			return
		}
		if u.Scope == "core" {
			old := e.r.Upgrade.Baseline[t.Name]
			if observed.Core == nil || observed.Core.ConfigSHA256 != old.CoreConfig || observed.Core.Genesis != old.CoreGenesis {
				return result, errors.New("Core upgrade response failed configuration/genesis preservation")
			}
		} else if !preservedCore(observed.Core, e.r.Upgrade.Baseline[t.Name]) {
			return result, errors.New("upgrade response failed Core preservation")
		} else if observed.Render == nil || !slices.Equal(observed.Render.Changes, e.r.Upgrade.Changes[t.Name]) {
			return result, fmt.Errorf("%s applied changes other than those staged", t.Name)
		}
		e.r.Upgrade.Phase = "verifying"
		e.report("upgrade-verifying")
		if err = e.save(); err != nil {
			return
		}
		if health, err = e.verifyUpgrade(); err != nil {
			return
		}
		e.r.Upgrade.Completed[t.Name] = true
		e.r.Upgrade.CurrentNode = ""
		if err = e.save(); err != nil {
			return
		}
		e.report("upgrade " + t.Name + " verified; whole fleet healthy")
	}
	if health.ObservedAt.IsZero() {
		if health, err = e.verifyUpgrade(); err != nil {
			return
		}
	}
	e.r.Upgrade.Phase = "complete"
	e.r.Upgrade.ObservedAt = health.ObservedAt
	if len(e.r.Upgrade.Sidecars) > 0 {
		e.r.Runtime.Sidecars = e.r.Upgrade.Sidecars
	}
	err = e.acceptHealth(health)
	return
}

// quietDKG is the part of the 24-block DKG cycle with no session running:
// every devnet session (including both rotation indexes) has finalized by
// block 13, and the next starts at 24. Mining stays paused while a validator
// is replaced, so no block (and no session) follows until it is back.
func quietDKG(height int64) bool { return height%24 >= 13 && height%24 <= 23 }

// cycleWait bounds a wait for a given part of the DKG cycle: a whole cycle at
// this network's block interval, and never less than ten minutes.
func (e *execution) cycleWait() time.Duration {
	return max(10*time.Minute, time.Duration(26*e.p.MiningIntervalSeconds)*time.Second)
}

// pauseMiningQuietly waits for the quiet part of the DKG cycle, then pauses
// the miner. A block mined between the observation and the pause is caught by
// checking the stopped height; mining then resumes until the next window.
func (e *execution) pauseMiningQuietly() error {
	deadline := time.Now().Add(e.cycleWait())
	for {
		o, err := e.call(e.p.Miner(), "core-status", nil)
		if err == nil && o.Core != nil && quietDKG(o.Core.Height) {
			if _, err = e.call(e.p.Miner(), "mine-pause", nil); err != nil {
				return err
			}
			if o, err = e.call(e.p.Miner(), "core-status", nil); err == nil && o.Core != nil && quietDKG(o.Core.Height) {
				e.report(fmt.Sprintf("mining paused at height %d (quiet DKG window) for the Core replacement", o.Core.Height))
				return nil
			}
			if err = e.resumeMining(); err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return errors.New("no quiet DKG window observed for the Core replacement")
		}
		if err = e.pause(2 * time.Second); err != nil {
			return err
		}
	}
}

// settle keeps blocks (and so DKG sessions) paused until the replaced
// validator is observably back: masternode READY, synced and connected, after
// at least 30 seconds for its quorum connections, within a bound.
func (e *execution) settle(t node.Target) error {
	if err := e.pause(30 * time.Second); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		o, err := e.call(t, "core-status", nil)
		if err == nil && o.Core != nil && o.Core.Synced && o.Core.MasternodeState == "READY" && o.Core.Peers >= 8 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not READY and connected after its Core replacement", t.Name)
		}
		if err = e.pause(3 * time.Second); err != nil {
			return err
		}
	}
}

// resumeMining uses its own bounded context so it still runs after the
// operation's context was cancelled or exhausted.
func (e *execution) resumeMining() error {
	cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	q := runtimeRequest(e.p, e.r, e.p.Miner(), "mine-start")
	q.PayoutAddress = e.r.Deployment.PayoutAddress
	if _, err := e.runner.Remote.Call(cleanup, q); err != nil {
		return fmt.Errorf("resume mining on %s: %w", e.p.Miner().Name, err)
	}
	e.report("mining resumed")
	return nil
}

func (e *execution) pause(d time.Duration) error {
	if e.runner.Wait != nil {
		return e.runner.Wait(e.ctx)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-e.ctx.Done():
		return e.ctx.Err()
	case <-timer.C:
		return nil
	}
}
