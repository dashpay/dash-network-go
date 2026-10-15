package managed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestReadinessWaitsForEvidenceNotFourMinuteFloor(t *testing.T) {
	p, r, b := migrationFixture(t)
	p.Migration.ReadinessDriven = true
	waits := 0
	b.observe = func(q Request, o *Observation) {
		if waits < 5 {
			o.Chain.CoreHeight = 200
			o.Chain.ChainLockHeight = 200
			o.Chain.PlatformHeight = 200
			o.Chain.DAPIHeight = 200
		}
	}
	r.Wait = func(ctx context.Context, d time.Duration) error {
		if d != 15*time.Second {
			t.Fatalf("fixed wait %s", d)
		}
		waits++
		if waits > 6 {
			return errors.New("too many waits")
		}
		return nil
	}
	ctx, c := deadline()
	defer c()
	h, e := r.planHealth(ctx, p)
	if e != nil || !h.Healthy || waits != 5 {
		t.Fatalf("slow block rejected or unnecessary waits: %v %d %v", e, waits, h.Problems)
	}
}

func TestReadinessImmediateEvidenceDoesNotSleep(t *testing.T) {
	p, r, _ := migrationFixture(t)
	p.Migration.ReadinessDriven = true
	r.Wait = func(context.Context, time.Duration) error { t.Fatal("sleep after evidence available"); return nil }
	ctx, c := deadline()
	defer c()
	h, e := r.planHealth(ctx, p)
	if e != nil || !h.Healthy {
		t.Fatal(e, h.Problems)
	}
}

func TestReadinessDeadlineAndIdentityRemainHardGates(t *testing.T) {
	for _, drift := range []bool{false, true} {
		p, r, b := migrationFixture(t)
		p.Migration.ReadinessDriven = true
		b.observe = func(q Request, o *Observation) {
			o.Chain.CoreHeight = 200
			o.Chain.PlatformHeight = 200
			o.Chain.DAPIHeight = 200
			if drift {
				o.Chain.ProTxHash = h64("foreign")
			}
		}
		r.Wait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
		ctx, c := deadline()
		h, e := r.planHealth(ctx, p)
		c()
		if e == nil || h.Healthy {
			t.Fatal("unsafe progress accepted", drift)
		}
	}
}

func TestReadinessFreshWithdrawalDoesNotRequireANewBlock(t *testing.T) {
	p, r, b := migrationFixture(t)
	p.Migration.ReadinessDriven = true
	b.observe = func(q Request, o *Observation) {
		o.Chain.CoreHeight = 200
		o.Chain.PlatformHeight = 200
		o.Chain.DAPIHeight = 200
	}
	r.Wait = func(context.Context, time.Duration) error { t.Fatal("withdrawal fixed wait"); return nil }
	ctx, c := deadline()
	defer c()
	h, e := r.withdrawalHealth(ctx, p)
	if e != nil || !h.Healthy {
		t.Fatal(e, h.Problems)
	}
}

type miningBackend struct {
	mu      sync.Mutex
	base    *backend
	paused  bool
	fail    string
	actions []string
	op      string
}

func (b *miningBackend) Call(ctx context.Context, q Request) (Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.actions = append(b.actions, q.Action)
	if q.Action == "mining_pause" {
		b.paused = true
		if b.fail == "lost-pause" {
			return Observation{}, errors.New("lost pause response")
		}
	}
	if q.Action == "mining_resume" {
		b.paused = false
	}
	if q.Action == "apply" && b.fail == "apply" {
		return Observation{}, errors.New("failed restart")
	}
	if q.Action == "mining_pause" || q.Action == "mining_status" || q.Action == "mining_resume" {
		return Observation{Mining: &MiningReceipt{OperationID: q.OperationID, Paused: b.paused, ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}, Chain: Chain{CoreHeight: 100, CoreHash: h64("tip")}}, nil
	}
	o, e := b.base.Call(ctx, q)
	if q.CoreCheckpointHeight > 0 {
		o.Chain.CoreCheckpointHash = h64("tip")
	}
	return o, e
}
func TestMiningRestoredOnSuccessAndAmbiguousFailure(t *testing.T) {
	for _, failure := range []string{"", "apply", "lost-pause"} {
		p, r, b := migrationFixture(t)
		p.Migration.ReadinessDriven = true
		p.Migration.Miners = []string{"miner"}
		for _, target := range p.Snapshot.Fleet.Targets {
			p.Targets = append(p.Targets, target.Name)
		}
		miner := Target{Name: "miner", Role: "core"}
		p.Snapshot.Fleet.Targets = append(p.Snapshot.Fleet.Targets, miner)
		remote := &miningBackend{base: b, fail: failure}
		r.Remote = remote
		target := p.Snapshot.Fleet.Targets[0]
		q := request(p.Snapshot.Fleet, target, "apply")
		q.Pins = p.Images[target.Name]
		ctx, c := deadline()
		e := r.applyWithMining(ctx, p, target, q, true)
		c()
		if remote.paused || remote.actions[len(remote.actions)-1] != "mining_resume" {
			t.Fatal("mining left paused", remote.actions)
		}
		if (e != nil) != (failure != "") {
			t.Fatal(failure, e)
		}
	}
}
func TestRecoveryDoesNotWaitForNewMiningWindow(t *testing.T) {
	p, r, b := migrationFixture(t)
	p.Migration.ReadinessDriven = true
	p.Migration.Miners = []string{"miner"}
	remote := &miningBackend{base: b}
	r.Remote = remote
	target := p.Snapshot.Fleet.Targets[0]
	q := request(p.Snapshot.Fleet, target, "apply")
	q.Pins = p.Images[target.Name]
	ctx, c := deadline()
	defer c()
	if e := r.applyWithMining(ctx, p, target, q, false); e != nil {
		t.Fatal(e)
	}
	for _, a := range remote.actions {
		if a == "admit" || a == "mining_pause" {
			t.Fatal("recovery blocked on new window")
		}
	}
}
