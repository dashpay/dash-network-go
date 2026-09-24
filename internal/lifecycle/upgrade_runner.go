package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
)

func preservedCore(c *node.Core, b provision.Preservation) bool {
	return c != nil && c.ContainerID == b.CoreID && c.StartedAt == b.CoreStarted && c.ConfigSHA256 == b.CoreConfig && c.Genesis == b.CoreGenesis
}

func upgradeEvidence(p Plan, record provision.Record, observed map[string]samples, pending string) (map[string]provision.Preservation, error) {
	baseline := map[string]provision.Preservation{}
	known := map[string]bool{}
	for _, t := range p.Validators() {
		known[record.Deployment.Nodes[t.Name].ProTxHash] = true
	}
	for _, t := range p.Targets {
		s := observed[t.Name]
		if s.core == nil || s.core.Genesis != record.Deployment.CoreGenesis {
			return nil, fmt.Errorf("missing/mismatched Core at %s", t.Name)
		}
		if _, err := time.Parse(time.RFC3339Nano, s.core.StartedAt); err != nil {
			return nil, fmt.Errorf("missing Core process start evidence at %s", t.Name)
		}
		b := provision.Preservation{CoreID: s.core.ContainerID, CoreStarted: s.core.StartedAt, CoreConfig: s.core.ConfigSHA256, CoreGenesis: s.core.Genesis}
		if record.Upgrade != nil && !preservedCore(s.core, record.Upgrade.Baseline[t.Name]) {
			return nil, fmt.Errorf("preserved Core changed at %s", t.Name)
		}
		if t.Role == "validator" {
			x := s.platform
			if x == nil {
				if t.Name == pending {
					baseline[t.Name] = b
					continue
				}
				return nil, fmt.Errorf("Platform observation missing at %s", t.Name)
			}
			if x.Protocol != p.InitialProtocolVersion || len(x.Validators) != 12 {
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
					if record.Runtime.Images[t.Name][component] == record.Upgrade.From[t.Name][component] && (x.Containers[component] != old.Containers[component] || x.Restarts[component] != old.Restarts[component]) {
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
	return q
}

func (e *execution) stageUpgrade(u UpgradePlan) error {
	// Cache images concurrently, but never withdraw a service in this phase.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	limit := make(chan struct{}, 4)
	for _, t := range e.p.Validators() {
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
			_, err := e.runner.Remote.Call(e.ctx, e.upgradeRequest(u, t, "upgrade-stage"))
			if err != nil {
				mu.Lock()
				failures = append(failures, t.Name+": "+node.SafeError(err))
				mu.Unlock()
			}
		}(t)
	}
	wg.Wait()
	if len(failures) > 0 {
		sort.Strings(failures)
		return errors.New("image staging failed: " + strings.Join(failures, "; "))
	}
	return nil
}

func (e *execution) verifyUpgrade() (Health, error) {
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Minute)
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
		e.r.Runtime = &provision.RuntimeState{DeploymentID: p.ID, UpgradeID: u.ID, Images: cloneImages(u.From)}
		e.r.Upgrade = &provision.UpgradeProgress{PlanID: u.ID, PreviousID: u.PreviousID, Phase: "staging", From: cloneImages(u.From), To: cloneImages(u.To), Baseline: baseline, Completed: map[string]bool{}}
		for _, t := range p.Validators() {
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
	e.report("stage exact upgrade images on all validators")
	e.report("upgrade-staging")
	if err = e.stageUpgrade(u); err != nil {
		return
	}
	var health Health
	if e.r.Upgrade.CurrentNode == "" {
		if health, err = e.verifyUpgrade(); err != nil {
			return
		}
	}
	// Recover the previously journaled withdrawal before scheduling any other.
	targets := p.Validators()
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
		e.report("upgrade " + t.Name + "; Core preserved")
		e.report("upgrade-applying")
		var observed node.Observation
		observed, err = r.Remote.Call(ctx, e.upgradeRequest(u, t, "upgrade-apply"))
		if err != nil {
			return
		}
		if !preservedCore(observed.Core, e.r.Upgrade.Baseline[t.Name]) {
			return result, errors.New("upgrade response failed Core preservation")
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
	err = e.acceptHealth(health)
	return
}
