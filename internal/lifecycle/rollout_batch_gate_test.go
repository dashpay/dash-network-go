package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/rollout"
)

func TestNativeBatchGatesAskForNoNewCoreBlock(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 3}); e != nil {
		t.Fatal(e)
	}
	// Between the first replacement and the last, no node sees a new Core
	// block. Only the gate after the last batch may ask for one.
	applies, total, waits := 0, len(f.plan.Validators()), 0
	between := func() bool { return applies > 0 && applies < total }
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "upgrade-apply" {
			applies++
		}
		if between() && o.Core != nil && q.Action == "core-status" {
			o.Core.Height, o.Core.Headers, o.Core.ChainLockHeight = 400, 400, 399
		}
		return nil
	}
	f.runner.Wait = func(ctx context.Context) error {
		f.remote.mu.Lock()
		if between() {
			waits++
		}
		f.remote.mu.Unlock()
		return ctx.Err()
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" || applies != total {
		t.Fatal(e, applies)
	}
	if waits != 0 {
		t.Fatal("a batch gate waited for a Core block", waits)
	}
}

func TestNativeFinalGateStillNeedsTheChainToAdvance(t *testing.T) {
	old := gateLimit
	gateLimit = 50 * time.Millisecond
	defer func() { gateLimit = old }()
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 3}); e != nil {
		t.Fatal(e)
	}
	f.runner.ReadyTimeout = time.Second
	applies, total := 0, len(f.plan.Validators())
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "upgrade-apply" {
			applies++
		}
		// Every batch passes, and then the chain never makes another block.
		if applies == total && o.Core != nil && q.Action == "core-status" {
			o.Core.Height, o.Core.Headers, o.Core.ChainLockHeight = 400, 400, 399
		}
		return nil
	}
	result, e := f.run(t, u)
	if e == nil || result.Upgrade.Phase == "complete" {
		t.Fatal("rollout completed on a chain that stopped")
	}
	// The gate names what it waited for, not the call its deadline interrupted.
	if !strings.Contains(e.Error(), "health gate not passed within") || !strings.Contains(e.Error(), "Core did not advance") {
		t.Fatal(e)
	}
}

func TestNativeGateSamplesAgainWhenANodeMissesOneProbe(t *testing.T) {
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 2}); e != nil {
		t.Fatal(e)
	}
	// A validator outside the batch fails exactly one probe, in the evidence
	// sample that follows a healthy gate after the first replacement.
	validators := f.plan.Validators()
	other := validators[len(validators)-1].Name
	applied, answers, missed := false, 0, false
	f.remote.before = func(q node.Request) error {
		applied = applied || q.Action == "upgrade-apply"
		if applied && !missed && q.Action == "platform-status" && q.Target.Name == other {
			// Baseline, gate, then the evidence sample.
			if answers++; answers == 3 {
				missed = true
				return errors.New("node platform-status:unsupported-validator-quorum: exit status 1")
			}
		}
		return nil
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" || !missed {
		t.Fatal(e, missed)
	}
}

func TestNativeReplacedNodesGetTheirReadyTimeNotTheShortGate(t *testing.T) {
	old := gateLimit
	gateLimit = 20 * time.Millisecond
	defer func() { gateLimit = old }()
	f := upgradeSetup(t)
	u := f.change(t, "platform", "b")
	if e := u.WithRollout(rollout.Options{Concurrency: 2}); e != nil {
		t.Fatal(e)
	}
	// The first replaced validator serves again only after the ordinary gate
	// limit has long passed; its batch gate allows it the ready timeout.
	f.runner.ReadyTimeout = 10 * time.Second
	first := ""
	var replaced time.Time
	f.hook = func(q node.Request, o *node.Observation) error {
		if q.Action == "upgrade-apply" && first == "" {
			first, replaced = q.Target.Name, time.Now()
		}
		if o.Platform != nil && q.Target.Name == first && time.Since(replaced) < 150*time.Millisecond {
			o.Platform.CatchingUp = true
		}
		return nil
	}
	f.runner.Wait = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Millisecond):
		}
		return ctx.Err()
	}
	result, e := f.run(t, u)
	if e != nil || result.Upgrade.Phase != "complete" {
		t.Fatal(e)
	}
}
