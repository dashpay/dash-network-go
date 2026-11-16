package managed

import (
	"context"
	"errors"
	"github.com/dashpay/dash-network-go/internal/rollout"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestRolloutBatchOverlapDurableIntentAndResume(t *testing.T) {
	p, r, m, b := stagingPlan(t, 5, "upgrade")
	if err := p.WithRollout(rollout.Options{Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	enroll(t, p.Snapshot, r)
	started := make(chan string, 5)
	release := make(chan struct{}, 5)
	var active, peak atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
		if q.Action != "apply" {
			return b.Call(ctx, q)
		}
		if !slices.Contains(m.r.PendingTargets, q.Target.Name) || len(m.r.PendingTargets) > 2 || m.r.Current != "" {
			t.Error("apply before whole batch intent", q.Target.Name)
		}
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- q.Target.Name
		select {
		case <-release:
		case <-ctx.Done():
		}
		defer active.Add(-1)
		o, e := b.Call(ctx, q)
		if fail.Swap(false) {
			return o, errors.New("lost apply response")
		}
		return o, e
	})
	ctx, c := deadline()
	defer c()
	done := make(chan error, 1)
	go func() { _, err := r.Execute(ctx, p); done <- err }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("no overlap")
		}
	}
	if len(m.r.PendingTargets) != 2 {
		t.Fatal("missing entire pending batch")
	}
	select {
	case <-started:
		t.Fatal("exceeded batch")
	case <-time.After(10 * time.Millisecond):
	}
	release <- struct{}{}
	release <- struct{}{}
	if err := <-done; err == nil || len(m.r.PendingTargets) != 2 || m.owner != "" || active.Load() != 0 {
		t.Fatal(err, m.r)
	}
	// Both ambiguous results are reconciled, without starting another batch first.
	r.Remote = b
	result, err := r.Execute(ctx, p)
	if err != nil || result.Phase != "complete" || len(result.PendingTargets) != 0 {
		t.Fatal(err, result)
	}
	if peak.Load() != 2 {
		t.Fatal("serial rollout", peak.Load())
	}
}

func TestRolloutAdmissionExcludesEntireBatch(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := Build(s, "upgrade", "platform", "", pins(s, "platform", "b"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = canWithdrawBatch(s, p, s.Fleet.Targets[:3]); err != nil {
		t.Fatal(err)
	}
	if err = canWithdrawBatch(s, p, s.Fleet.Targets[:4]); err == nil {
		t.Fatal("accepted loss of 1/3 voting power")
	}
}

func TestRolloutMiningCleanupAndNoConfigMigration(t *testing.T) {
	for _, failure := range []string{"", "mining_pause", "apply"} {
		t.Run(failure, func(t *testing.T) {
			p, r, b := fleetFixture(t)
			p.Migration = nil
			if err := p.WithRollout(rollout.Options{Concurrency: 2, PauseMining: true, Miners: []string{"miner-1", "miner-2"}}); err != nil {
				t.Fatal(err)
			}
			b.failure = failure
			ctx, c := deadline()
			defer c()
			record, err := r.Execute(ctx, p)
			if (failure == "") != (err == nil) || len(b.paused) != 0 || record.MiningPhase != "resumed" {
				t.Fatal(err, record.Phase, record.MiningPhase, b.paused)
			}
			pauses, resumes, applies := 0, 0, 0
			for _, q := range b.calls {
				switch q.Action {
				case "mining_pause":
					pauses++
				case "mining_resume":
					resumes++
				case "apply":
					applies++
					if q.Migration != nil || pauses != 2 || resumes != 0 {
						t.Fatal("upgrade escaped owned pause or opted into config migration")
					}
				}
			}
			if failure == "" && (applies != 29 || resumes != 2) {
				t.Fatal(applies, resumes)
			}
		})
	}
}

func TestRolloutBindsOptionsAndPreservesOldPlan(t *testing.T) {
	p, _, _, _ := stagingPlan(t, 2, "upgrade")
	old := p.ID
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := p.WithRollout(rollout.Options{Concurrency: 2}); err != nil || p.ID == old {
		t.Fatal(err)
	}
	p.Rollout.Concurrency = 3
	if p.Validate() == nil {
		t.Fatal("unreviewed concurrency accepted")
	}
	for _, n := range []int{-1, 0, 17} {
		if (rollout.Options{Concurrency: n}).Validate() == nil {
			t.Fatal(n)
		}
	}
}

func TestRolloutDoesNotScanUnrelatedNodesOrWaitFixedWindows(t *testing.T) {
	p, r, _, b := stagingPlan(t, 6, "upgrade")
	chosen := p.Snapshot.Fleet.Targets[0].Name
	var err error
	for _, target := range p.Snapshot.Fleet.Targets[1:] {
		p.Images[target.Name] = map[string]string{}
	}
	p, err = BuildSelected(p.Snapshot, "upgrade", "core", "", p.Images, []string{chosen}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = p.WithRollout(rollout.Options{Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	enroll(t, p.Snapshot, r)
	b.calls = nil
	r.Window = 4 * time.Minute
	r.Wait = func(context.Context, time.Duration) error { t.Error("ready targets incurred a fixed wait"); return nil }
	ctx, c := deadline()
	defer c()
	if _, err = r.Execute(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, q := range b.calls {
		if q.Target.Name != chosen {
			t.Fatal("unrelated host on upgrade critical path", q.Target.Name, q.Action)
		}
	}
}

func TestRolloutCancellationRestoresMining(t *testing.T) {
	p, r, b := fleetFixture(t)
	p.Migration = nil
	if e := p.WithRollout(rollout.Options{Concurrency: 2, PauseMining: true, Miners: []string{"miner-1", "miner-2"}}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := deadline()
	defer cancel()
	r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
		if q.Action == "apply" {
			cancel()
		}
		return b.Call(ctx, q)
	})
	record, e := r.Execute(ctx, p)
	if !errors.Is(e, context.Canceled) || len(b.paused) != 0 || record.MiningPhase != "resumed" {
		t.Fatal(e, b.paused, record.MiningPhase)
	}
}
