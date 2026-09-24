package lifecycle

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
	"github.com/google/go-containerregistry/pkg/name"
)

type upgradeFixture struct {
	plan      Plan
	runner    Runner
	store     *memoryStore
	remote    *fakeRemote
	installed provision.FleetImages
	hook      func(node.Request, *node.Observation) error
}

func upgradeSetup(t *testing.T) *upgradeFixture {
	t.Helper()
	p, r, s, f := setup(t)
	installed, err := effectiveImages(p, s.record)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &upgradeFixture{plan: p, runner: r, store: s, remote: f, installed: installed}
	f.after = func(q node.Request, o *node.Observation) error {
		if o.Core != nil {
			o.Core.StartedAt = "2026-09-24T10:00:00Z"
		}
		if q.Action == "upgrade-apply" {
			fixture.installed[q.Target.Name] = maps.Clone(q.Upgrade.To)
		}
		if q.Action == "upgrade-stage" || q.Action == "upgrade-apply" {
			b := q.Upgrade.Preserve
			o.Core = &node.Core{ContainerID: b.CoreID, StartedAt: b.CoreStarted, ConfigSHA256: b.CoreConfig, Genesis: b.CoreGenesis}
		}
		if o.Platform != nil {
			o.Platform.Protocol = 14
			for _, t := range p.Validators()[:12] {
				o.Platform.Validators = append(o.Platform.Validators, digest("protx"+t.Name))
			}
			o.Platform.Restarts = map[string]int{}
			for _, image := range q.Target.Images {
				if image.Component == "core" || image.Component == "helper" {
					continue
				}
				if fixture.installed[q.Target.Name][image.Component] != image.Pinned {
					return errors.New("actual image differs from runtime intent")
				}
				o.Platform.Containers[image.Component] = digest(q.Target.Name + image.Component + image.Pinned)
				o.Platform.Restarts[image.Component] = 0
			}
		}
		if fixture.hook != nil {
			return fixture.hook(q, o)
		}
		return nil
	}
	if _, err := execute(t, p, r); err != nil {
		t.Fatal(err)
	}
	return fixture
}
func (f *upgradeFixture) change(t *testing.T, scope, letter string) UpgradePlan {
	t.Helper()
	candidate := f.plan.Bootstrap.Compute.Network
	candidate.Images = maps.Clone(candidate.Images)
	for component, ref := range candidate.Images {
		parsed, err := name.ParseReference(ref)
		if err != nil {
			t.Fatal(err)
		}
		candidate.Images[component] = parsed.Context().Tag("upgrade-" + letter).Name()
	}
	lock := testutil.Lock(t, candidate)
	for i := range lock.Images {
		image := &lock.Images[i]
		ref, _ := name.NewDigest(image.Pinned)
		pin := "sha256:" + strings.Repeat(letter, 64)
		image.Pinned = ref.Context().Digest(pin).Name()
		for j := range image.Platforms {
			image.Platforms[j].Digest = pin
		}
	}
	u, err := BuildUpgrade(f.plan, f.store.record, candidate, lock, scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func (f *upgradeFixture) run(t *testing.T, u UpgradePlan) (provision.Record, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return f.runner.Upgrade(ctx, u)
}
func callsFor(f *fakeRemote, action string) int {
	n := 0
	for _, q := range f.calls {
		if q.Action == action {
			n++
		}
	}
	return n
}

func TestUpgradeMissingObservationRetainsSafeFailureReason(t *testing.T) {
	f := upgradeSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	observed := f.runner.sample(ctx, f.plan, f.store.record, 0)
	target := f.plan.Validators()[4].Name
	s := observed[target]
	s.platform = nil
	s.problems = []string{"Platform: node platform-status:dapi-unavailable; raw output withheld"}
	observed[target] = s
	_, err := upgradeEvidence(f.plan, f.store.record, observed, "")
	if err == nil || !strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), s.problems[0]) {
		t.Fatalf("missing useful sanitized preflight cause: %v", err)
	}
	// The exact unfinished target is still recoverable; a missing observation
	// on an unrelated node must not receive the same exception.
	if _, err := upgradeEvidence(f.plan, f.store.record, observed, target); err != nil {
		t.Fatal("pending target cannot be reconciled", err)
	}
	if _, err := upgradeEvidence(f.plan, f.store.record, observed, f.plan.Validators()[0].Name); err == nil {
		t.Fatal("unrelated missing target was ignored")
	}
}

func TestUpgradeOneValidatorAtATimeAndRetainedRuntime(t *testing.T) {
	for _, scope := range []string{"platform", "tenderdash"} {
		t.Run(scope, func(t *testing.T) {
			f := upgradeSetup(t)
			u := f.change(t, scope, "b")
			f.remote.calls = nil
			last := ""
			f.hook = func(q node.Request, o *node.Observation) error {
				if q.Action == "upgrade-apply" {
					if last != "" && !f.store.record.Upgrade.Completed[last] {
						t.Error("withdrew another node before previous health gate")
					}
					if f.store.record.Upgrade.CurrentNode != q.Target.Name || f.store.record.Runtime.Images[q.Target.Name]["tenderdash"] != u.To[q.Target.Name]["tenderdash"] {
						t.Error("remote action preceded durable intent")
					}
					last = q.Target.Name
				}
				return nil
			}
			result, err := f.run(t, u)
			if err != nil {
				t.Fatal(err)
			}
			if result.Upgrade.Phase != "complete" || len(result.Upgrade.Completed) != 13 || f.store.owner != "" {
				t.Fatal("incomplete rollout", result.Upgrade)
			}
			if callsFor(f.remote, "upgrade-stage") != 13 || callsFor(f.remote, "upgrade-apply") != 13 {
				t.Fatal("lost intended validator")
			}
			for _, q := range f.remote.calls {
				if q.Action != "upgrade-stage" && q.Action != "upgrade-apply" && q.Action != "core-status" && q.Action != "platform-status" {
					t.Fatal("upgrade used lifecycle mutation", q.Action)
				}
				for _, image := range q.Target.Images {
					if image.Component == "core" && image.Pinned != u.From[q.Target.Name]["core"] {
						t.Fatal("Core pin changed")
					}
				}
			}
			for node, images := range result.Runtime.Images {
				for component, pin := range images {
					if component == "core" || (scope == "tenderdash" && component != "tenderdash") {
						if pin != u.From[node][component] {
							t.Fatal("unselected image changed")
						}
					}
				}
			}
			f.hook = nil
			f.remote.calls = nil
			if _, err := f.run(t, u); err != nil {
				t.Fatal("completed replay", err)
			}
			if callsFor(f.remote, "upgrade-apply") != 0 {
				t.Fatal("completed replay mutated")
			}
			// Ordinary restart/recovery must use the recorded new images, not creation pins.
			if _, err := execute(t, f.plan, f.runner); err != nil {
				t.Fatal("upgraded deployment cannot be verified", err)
			}
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
			if _, err := f.runner.Execute(stopCtx, f.plan, true); err != nil {
				t.Fatal("stop after upgrade", err)
			}
			stopCancel()
			f.remote.calls = nil
			if _, err := execute(t, f.plan, f.runner); err != nil {
				t.Fatal("resume after upgrade", err)
			}
			starts := 0
			for _, q := range f.remote.calls {
				if q.Action != "platform-start" {
					continue
				}
				starts++
				for _, image := range q.Target.Images {
					if image.Pinned != u.To[q.Target.Name][image.Component] {
						t.Fatal("resume reverted creation-time image", q.Target.Name, image.Component)
					}
				}
			}
			if starts != 13 {
				t.Fatal("resume lost an upgraded validator", starts)
			}
			next := f.change(t, scope, "c")
			if _, err := f.run(t, next); err != nil {
				t.Fatal("second rollout", err)
			}
			count := callsFor(f.remote, "upgrade-apply")
			if _, err := f.run(t, u); err == nil {
				t.Fatal("old plan accepted after another rollout")
			}
			if callsFor(f.remote, "upgrade-apply") != count {
				t.Fatal("stale plan changed services")
			}
		})
	}
}

func TestUpgradeLostSSHResponseResumesOnlyPendingNodeFirst(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	f.remote.calls = nil
	first := f.plan.Validators()[0].Name
	lost := false
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "upgrade-apply" && !lost {
			lost = true
			return errors.New("accepted remotely; response lost")
		}
		return nil
	}
	result, err := f.run(t, u)
	if err == nil || result.Upgrade.CurrentNode != first || result.Upgrade.Phase != "interrupted" || callsFor(f.remote, "upgrade-apply") != 1 || f.store.owner != "" {
		t.Fatal("lost response not checkpointed", result.Upgrade, err)
	}
	if _, err := execute(t, f.plan, f.runner); err == nil {
		t.Fatal("deployment bypassed unfinished rollout")
	}
	f.hook = nil
	f.remote.calls = nil
	result, err = f.run(t, u)
	if err != nil || result.Upgrade.Phase != "complete" || result.LastError != "" {
		t.Fatal("resume failed", err)
	}
	for _, q := range f.remote.calls {
		if q.Action == "upgrade-apply" {
			if q.Target.Name != first {
				t.Fatal("another node changed before reconciliation")
			}
			break
		}
	}
}

func TestUpgradeNeverProceedsPastCoreRestartOrUnhealthyFleet(t *testing.T) {
	for _, failure := range []string{"unreachable", "foreign-member", "protocol", "core-restart"} {
		t.Run(failure, func(t *testing.T) {
			f := upgradeSetup(t)
			u := f.change(t, "platform", "b")
			f.remote.calls = nil
			applied := false
			f.hook = func(q node.Request, o *node.Observation) error {
				if q.Action == "upgrade-apply" {
					applied = true
				}
				if failure == "unreachable" && q.Target.Name == f.plan.Targets[0].Name {
					return errors.New("host unreachable")
				}
				if o.Platform != nil && failure == "foreign-member" {
					o.Platform.Validators[0] = digest("foreign")
				}
				if o.Platform != nil && failure == "protocol" {
					o.Platform.Protocol = 15
				}
				if o.Core != nil && failure == "core-restart" && applied {
					o.Core.StartedAt = "2026-09-24T11:00:00Z"
				}
				return nil
			}
			_, err := f.run(t, u)
			if err == nil {
				t.Fatal("failure accepted")
			}
			want := 0
			if failure == "core-restart" {
				want = 1
			}
			if got := callsFor(f.remote, "upgrade-apply"); got != want {
				t.Fatal("unsafe further withdrawals", got, want)
			}
		})
	}
}

func TestUpgradePlanCannotChangeScopeIdentityOrProtocol(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	for _, alter := range []func(*UpgradePlan){
		func(u *UpgradePlan) { u.Scope = "core" },
		func(u *UpgradePlan) { u.Candidate.Metadata.Name = "devnet-foreign" },
		func(u *UpgradePlan) {
			u.To[f.plan.Targets[0].Name]["core"] = "docker.io/dashpay/dashd@sha256:" + strings.Repeat("f", 64)
		},
		func(u *UpgradePlan) { delete(u.From, f.plan.Targets[0].Name) },
	} {
		copy := u
		copy.From = cloneImages(u.From)
		copy.To = cloneImages(u.To)
		alter(&copy)
		copy.ID = ""
		copy.ID = hash(copy)
		if err := copy.Validate(); err == nil {
			t.Fatal("altered plan accepted")
		}
	}
	f.store.owner = "other-runner"
	f.remote.calls = nil
	if _, err := f.run(t, u); err == nil || len(f.remote.calls) != 0 {
		t.Fatal("competing runner reached hosts", err)
	}
}

func TestUpgradeIntentSavedBeforeUnobservedApplyAndStagingFailure(t *testing.T) {
	for _, failure := range []string{"before-apply", "staging"} {
		t.Run(failure, func(t *testing.T) {
			f := upgradeSetup(t)
			u := f.change(t, "platform", "b")
			f.remote.calls = nil
			failed := false
			f.remote.before = func(q node.Request) error {
				action := "upgrade-apply"
				if failure == "staging" {
					action = "upgrade-stage"
				}
				if q.Action == action && !failed {
					failed = true
					return errors.New("connection lost before command reached host")
				}
				return nil
			}
			result, err := f.run(t, u)
			if err == nil {
				t.Fatal("expected interruption")
			}
			if failure == "staging" && callsFor(f.remote, "upgrade-apply") != 0 {
				t.Fatal("staging failure withdrew a service")
			}
			if failure == "before-apply" && result.Upgrade.CurrentNode == "" {
				t.Fatal("unobserved apply lost intent")
			}
			if f.store.owner != "" {
				t.Fatal("claim not released")
			}
			f.remote.before = nil
			if _, err := f.run(t, u); err != nil {
				t.Fatal("cannot reconcile unobserved request", err)
			}
		})
	}
}
