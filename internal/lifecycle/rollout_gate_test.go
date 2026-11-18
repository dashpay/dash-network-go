package lifecycle

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/rollout"
)

// leased answers mining control calls like the host: a receipt for this
// operation that follows the pause state the test tracks.
func leased(u UpgradePlan, paused *bool, q node.Request, o *node.Observation) {
	switch q.Action {
	case "rollout-mining-pause":
		*paused = true
	case "rollout-mining-resume":
		*paused = false
	}
	if q.MiningControl != nil {
		o.MiningPause = &node.MiningPause{OperationID: u.ID, ContainerID: q.MiningControl.ContainerID, Paused: *paused, ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}
	}
	if o.Core != nil && o.Core.Mining != nil {
		o.Core.Mining.Running = !*paused
	}
}

func TestNativeMiningControlRetriesABusyHost(t *testing.T) {
	old := hostBusyRetry
	hostBusyRetry = time.Millisecond
	defer func() { hostBusyRetry = old }()
	for _, busy := range []int{2, hostBusyAttempts} {
		f := upgradeSetup(t)
		u := f.change(t, "platform", "b")
		if e := u.WithRollout(rollout.Options{Concurrency: 2, PauseMining: true}); e != nil {
			t.Fatal(e)
		}
		paused, refused := false, 0
		f.hook = func(q node.Request, o *node.Observation) error {
			if q.Action == "rollout-mining-status" && refused < busy {
				refused++
				return errors.New("node host-busy: exit status 1")
			}
			leased(u, &paused, q, o)
			return nil
		}
		_, e := f.run(t, u)
		// A sample holding the host lock for a moment is not a lost lease; a
		// host that never frees it still stops the rollout, with mining restored.
		if (busy < hostBusyAttempts) != (e == nil) || paused {
			t.Fatal(busy, e, paused)
		}
	}
}

func TestNativeCoreBatchSettlesBeforeTheNextBatch(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "core", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 2, PauseMining: true}); e != nil {
		t.Fatal(e)
	}
	paused := false
	reconnecting, connected := map[string]int{}, map[string]bool{}
	f.hook = func(q node.Request, o *node.Observation) error {
		leased(u, &paused, q, o)
		if o.Core != nil && q.Action == "core-status" && q.Target.Name == f.plan.Miner().Name {
			if phase := f.store.record.Upgrade; phase != nil && (phase.MiningPhase == "pausing" || paused) {
				o.Core.Height, o.Core.Headers = 517, 517
			}
		}
		if q.Action == "upgrade-apply" || q.Action == "rollout-mining-resume" {
			for name := range reconnecting {
				if !connected[name] && !slices.Contains(f.store.record.Upgrade.PendingNodes, name) {
					t.Errorf("%s reached before %s reconnected", q.Action, name)
				}
			}
		}
		if q.Action == "upgrade-apply" && q.Target.Role == "validator" {
			reconnecting[q.Target.Name] = 3
		}
		// A replaced Core is READY with a single peer well before its quorum
		// connections are back.
		if q.Action == "core-status" && o.Core != nil {
			if left, ok := reconnecting[q.Target.Name]; ok && left > 0 {
				reconnecting[q.Target.Name] = left - 1
				o.Core.Peers = 1
			} else if ok {
				connected[q.Target.Name] = true
			}
		}
		return nil
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" || paused || len(connected) != len(f.plan.Validators()) {
		t.Fatal(e, paused, len(connected))
	}
}

func TestNativeJournaledBatchIsNotWithdrawnFromADegradedFleet(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 2}); e != nil {
		t.Fatal(e)
	}
	lost := true
	f.remote.before = func(q node.Request) error {
		if q.Action == "upgrade-apply" && lost {
			return errors.New("lost before the host")
		}
		return nil
	}
	if _, e := f.run(t, u); e == nil || len(f.store.record.Upgrade.PendingNodes) != 2 {
		t.Fatal("batch not journaled", e)
	}
	lost = false
	pending := f.store.record.Upgrade.PendingNodes
	var degraded []string
	for _, v := range f.plan.Validators()[:12] {
		if !slices.Contains(pending, v.Name) && len(degraded) < 3 {
			degraded = append(degraded, v.Name)
		}
	}
	applies := 0
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "upgrade-apply" {
			applies++
		}
		if o.Platform != nil && slices.Contains(degraded, q.Target.Name) {
			o.Platform.CatchingUp = true
		}
		return nil
	}
	// Both journaled validators still serve. With three others behind, taking
	// them now would leave seven of twelve.
	if _, e := f.run(t, u); e == nil || !strings.Contains(e.Error(), "journaled batch not resumed") || applies != 0 {
		t.Fatal(e, applies)
	}
	degraded = nil
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" {
		t.Fatal(e)
	}
}

func TestNativeJournaledPauseIsRestoredWhenOnlyTheMinerRemains(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "core", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 2, PauseMining: true}); e != nil {
		t.Fatal(e)
	}
	paused, refuse := false, 2
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "rollout-mining-resume" && refuse > 0 {
			refuse--
			return errors.New("connection lost")
		}
		leased(u, &paused, q, o)
		if o.Core != nil && q.Action == "core-status" && q.Target.Name == f.plan.Miner().Name {
			if phase := f.store.record.Upgrade; phase != nil && (phase.MiningPhase == "pausing" || paused) {
				o.Core.Height, o.Core.Headers = 517, 517
			}
		}
		if q.Action == "upgrade-apply" && q.Target.Name == f.plan.Miner().Name && paused {
			t.Error("miner replaced while its generation was still held")
		}
		return nil
	}
	// The controller dies between the last validator batch and the resume.
	if _, e := f.run(t, u); e == nil || !paused || f.store.record.Upgrade.Completed[f.plan.Miner().Name] {
		t.Fatal("interruption not constructed", e, paused)
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" || paused {
		t.Fatal(e, paused)
	}
}

func TestNativeReadinessRetakesABaselineThatMissedANode(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 1}); e != nil {
		t.Fatal(e)
	}
	applied, missed := false, false
	f.remote.before = func(q node.Request) error {
		// The first observation after the first replacement opens that
		// batch's readiness baseline.
		if q.Action == "core-status" && applied && !missed {
			missed = true
			return errors.New("node host-busy: exit status 1")
		}
		return nil
	}
	f.hook = func(q node.Request, o *node.Observation) error {
		applied = applied || q.Action == "upgrade-apply"
		return nil
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" || !missed {
		t.Fatal(e, missed)
	}
}
