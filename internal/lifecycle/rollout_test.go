package lifecycle

import (
	"context"
	"errors"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/rollout"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type rolloutBackend func(context.Context, node.Request) (node.Observation, error)

func (f rolloutBackend) Call(ctx context.Context, q node.Request) (node.Observation, error) {
	return f(ctx, q)
}

func TestNativeRolloutOverlapDurableBatchAndResume(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if err := u.WithRollout(rollout.Options{Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	starts := make(chan string, 16)
	release := make(chan struct{}, 16)
	var active, peak atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	f.runner.Remote = rolloutBackend(func(ctx context.Context, q node.Request) (node.Observation, error) {
		if q.Action != "upgrade-apply" {
			return f.remote.Call(ctx, q)
		}
		if !slices.Contains(f.store.record.Upgrade.PendingNodes, q.Target.Name) || len(f.store.record.Upgrade.PendingNodes) != 2 {
			t.Error("whole batch not journaled")
		}
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		starts <- q.Target.Name
		select {
		case <-release:
		case <-ctx.Done():
		}
		defer active.Add(-1)
		o, e := f.remote.Call(ctx, q)
		if fail.Swap(false) {
			return o, errors.New("lost response")
		}
		return o, e
	})
	ctx, c := context.WithTimeout(context.Background(), 15*time.Second)
	defer c()
	done := make(chan error, 1)
	go func() { _, e := f.runner.Upgrade(ctx, u); done <- e }()
	for range 2 {
		select {
		case <-starts:
		case <-ctx.Done():
			t.Fatal("no overlap")
		}
	}
	select {
	case <-starts:
		t.Fatal("batch bound exceeded")
	case <-time.After(10 * time.Millisecond):
	}
	release <- struct{}{}
	release <- struct{}{}
	if e := <-done; e == nil || active.Load() != 0 || len(f.store.record.Upgrade.PendingNodes) != 2 {
		t.Fatal(e)
	}
	f.runner.Remote = f.remote
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" || peak.Load() != 2 {
		t.Fatal(e, result.Upgrade.Phase)
	}
}

func TestNativeBatchQuorumCapsRequestedMaximum(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 16}); e != nil {
		t.Fatal(e)
	}
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "upgrade-apply" {
			votes := 0
			for _, name := range f.store.record.Upgrade.PendingNodes {
				for _, v := range f.plan.Validators()[:12] {
					if v.Name == name {
						votes++
					}
				}
			}
			if votes > 3 {
				t.Error("unsafe batch admitted")
			}
		}
		return nil
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" {
		t.Fatal(e)
	}
}

func TestNativeOwnedPauseResumesOnSuccessAndFailure(t *testing.T) {
	for _, failure := range []string{"", "rollout-mining-pause", "upgrade-apply"} {
		t.Run(failure, func(t *testing.T) {
			f := upgradeSetup(t)
			u := f.change(t, "platform", "b")
			if e := u.WithRollout(rollout.Options{Concurrency: 2, PauseMining: true}); e != nil {
				t.Fatal(e)
			}
			paused, pauses, resumes, applies := false, 0, 0, 0
			f.hook = func(q node.Request, o *node.Observation) error {
				switch q.Action {
				case "rollout-mining-pause":
					paused = true
					pauses++
				case "rollout-mining-resume":
					paused = false
					resumes++
				case "upgrade-apply":
					applies++
					if !paused {
						t.Error("apply outside pause")
					}
				}
				if q.MiningControl != nil {
					o.MiningPause = &node.MiningPause{OperationID: u.ID, ContainerID: q.MiningControl.ContainerID, Paused: paused, ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}
				}
				if o.Core != nil && o.Core.Mining != nil {
					o.Core.Mining.Running = !paused
				}
				if q.Action == failure {
					return errors.New("ambiguous failure")
				}
				return nil
			}
			_, e := f.run(t, u)
			if (failure == "") != (e == nil) || paused || resumes != 1 || pauses != 1 {
				t.Fatal(e, paused, pauses, resumes)
			}
			if failure == "" && applies != len(f.plan.Validators()) {
				t.Fatal(applies)
			}
		})
	}
}

func TestNativeCoreRolloutMinerLastAndAlone(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "core", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 2, PauseMining: true}); e != nil {
		t.Fatal(e)
	}
	paused := false
	seen := map[string]bool{}
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "rollout-mining-pause" {
			paused = true
		}
		if q.Action == "rollout-mining-resume" {
			paused = false
		}
		if q.MiningControl != nil {
			o.MiningPause = &node.MiningPause{OperationID: u.ID, ContainerID: q.MiningControl.ContainerID, Paused: paused, ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}
		}
		if o.Core != nil && q.Action == "core-status" && q.Target.Name == f.plan.Miner().Name {
			if phase := f.store.record.Upgrade; phase != nil && (phase.MiningPhase == "pausing" || paused) {
				o.Core.Height = 517
				o.Core.Headers = 517
			}
			o.Core.Mining.Running = !paused
		}
		if q.Action == "upgrade-apply" {
			if q.Target.Name == f.plan.Miner().Name {
				if paused || len(seen) != len(f.plan.Targets)-1 || len(f.store.record.Upgrade.PendingNodes) != 1 {
					t.Error("miner not final isolated batch")
				}
			}
			seen[q.Target.Name] = true
		}
		return nil
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" || paused {
		t.Fatal(e, paused)
	}
}

func TestNativePendingCoreEvidenceCanRecoverAllMissingMembers(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "core", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 2}); e != nil {
		t.Fatal(e)
	}
	// Construct the interrupted persisted batch at its public evidence boundary.
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	before := f.runner.sample(ctx, f.plan, f.store.record, 0)
	baseline, e := upgradeEvidence(f.plan, f.store.record, before, "")
	if e != nil {
		t.Fatal(e)
	}
	r := f.store.record
	r.Upgrade = &provision.UpgradeProgress{Scope: "core", Baseline: baseline, Completed: map[string]bool{}, PendingNodes: []string{f.plan.Validators()[0].Name, f.plan.Validators()[1].Name}}
	for _, name := range r.Upgrade.PendingNodes {
		s := before[name]
		s.core = nil
		s.platform = nil
		before[name] = s
	}
	if _, e = upgradeEvidence(f.plan, r, before, ""); e != nil {
		t.Fatal("pending batch unrecoverable", e)
	}
}
