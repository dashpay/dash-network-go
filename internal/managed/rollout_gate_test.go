package managed

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/rollout"
)

func TestRolloutJournaledBatchIsNotWithdrawnFromADegradedFleet(t *testing.T) {
	p, r, b := fleetFixture(t)
	p.Migration = nil
	if err := p.WithRollout(rollout.Options{Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	// The canary completes; one member of the first full batch is then lost
	// before its host and keeps serving.
	var attempts atomic.Int32
	r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
		if q.Action == "apply" && attempts.Add(1) == 2 {
			return Observation{}, errors.New("ambiguous apply")
		}
		return b.Call(ctx, q)
	})
	ctx, c := deadline()
	defer c()
	record, err := r.Execute(ctx, p)
	r.Remote = b
	if err == nil || len(record.PendingTargets) != 2 {
		t.Fatal("batch not journaled", err, record.PendingTargets)
	}
	var degraded []string
	for _, target := range p.Snapshot.Fleet.Targets {
		if target.Role == "validator" && !slices.Contains(record.PendingTargets, target.Name) && len(degraded) < 12 {
			degraded = append(degraded, target.Name)
		}
	}
	previous := b.base.observe
	b.base.observe = func(q Request, o *Observation) {
		previous(q, o)
		o.Problems = nil
		if slices.Contains(degraded, q.Target.Name) {
			o.Problems = []string{"platform-health-unavailable"}
		}
	}
	applies := func() (n int) {
		for _, q := range b.calls {
			if q.Action == "apply" {
				n++
			}
		}
		return
	}
	before := applies()
	if _, err = r.Execute(ctx, p); err == nil || !strings.Contains(err.Error(), "journaled batch not resumed") || applies() != before {
		t.Fatal(err, applies()-before)
	}
	degraded = nil
	record, err = r.Execute(ctx, p)
	if err != nil || record.Phase != "complete" || len(record.PendingTargets) != 0 {
		t.Fatal(err, record.Phase)
	}
}

func TestReadinessOutlastsAHostItCouldNotObserve(t *testing.T) {
	for _, round := range []int{1, 2} {
		p, r, b := migrationFixture(t)
		p.Migration.ReadinessDriven = true
		name := p.Snapshot.Fleet.Targets[0].Name
		seen := 0
		b.observe = func(q Request, o *Observation) {
			if q.Target.Name != name {
				return
			}
			seen++
			o.Problems = nil
			// One failed probe, in the baseline or in a later poll: the host
			// reports no chain at all, which is neither a fork nor a stall.
			if seen == round {
				o.Problems = []string{"platform-health-unavailable"}
				o.Chain.PlatformHash = ""
			}
		}
		waits := 0
		r.Wait = func(context.Context, time.Duration) error {
			if waits++; waits > 4 {
				return context.DeadlineExceeded
			}
			return nil
		}
		ctx, c := deadline()
		h, e := r.planHealth(ctx, p)
		c()
		if e != nil || !h.Healthy {
			t.Fatal(round, e, h.Problems)
		}
	}
}

func TestFleetLeaseAsksAgainWhenABlockLandsBetweenMiners(t *testing.T) {
	for _, forked := range []bool{false, true} {
		p, r, b := fleetFixture(t)
		b.paused = map[string]bool{"miner-1": true, "miner-2": true}
		landed, calls := false, 0
		r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
			o, e := b.Call(ctx, q)
			if q.Action != "mining_status" {
				return o, e
			}
			calls++
			if q.Target.Name == "miner-2" && (forked || !landed) {
				landed = true
				o.Chain.CoreHeight, o.Chain.CoreHash = 2001, h64("fork")
				if !forked {
					o.Chain.CoreHash = h64("next")
				}
			} else if landed && !forked {
				o.Chain.CoreHeight, o.Chain.CoreHash = 2001, h64("next")
			}
			return o, e
		})
		ctx, c := deadline()
		tip, _, e := (&fleetLease{r: r, p: p}).check(ctx, false)
		c()
		// The public chain moved between two answers; a second sweep sees one
		// tip. Miners that keep disagreeing still end the lease.
		if forked != (e != nil) || (!forked && (tip != 2001 || calls != 4)) || (forked && calls != 6) {
			t.Fatal(forked, tip, calls, e)
		}
	}
}

func TestReadinessWaitsOutDAPILagButStopsOnAFallAcrossAFailedProbe(t *testing.T) {
	for _, fall := range []bool{false, true} {
		p, r, b := migrationFixture(t)
		p.Migration.ReadinessDriven = true
		name := p.Snapshot.Fleet.Targets[0].Name
		seen := 0
		var reached, core int64
		b.observe = func(q Request, o *Observation) {
			if q.Target.Name != name {
				return
			}
			seen++
			o.Problems = nil
			o.Chain.DAPIHeight = o.Chain.PlatformHeight
			switch {
			case seen == 1:
				core = o.Chain.CoreHeight
			case seen == 2 && !fall:
				o.Chain.PlatformHeight += 8 // DAPI behind, and able to catch up
			case seen == 2:
				// Platform has risen; Core has not, so the gate keeps waiting.
				o.Chain.CoreHeight, reached = core, o.Chain.PlatformHeight
			case seen == 3 && fall:
				o.Problems = []string{"platform-health-unavailable"}
				o.Chain.PlatformHeight, o.Chain.DAPIHeight, o.Chain.PlatformHash = 0, 0, ""
			case seen == 4 && fall:
				o.Chain.PlatformHeight, o.Chain.DAPIHeight = reached-5, reached-5
			}
		}
		waits := 0
		r.Wait = func(context.Context, time.Duration) error {
			if waits++; waits > 6 {
				return context.DeadlineExceeded
			}
			return nil
		}
		ctx, c := deadline()
		h, e := r.planHealth(ctx, p)
		c()
		if fall != (e != nil) || (fall && !strings.Contains(e.Error(), "not advancing")) || (!fall && !h.Healthy) {
			t.Fatal(fall, e, h.Problems)
		}
	}
}

func TestFleetLeaseNeedsACheckpointFromEveryMiner(t *testing.T) {
	p, r, b := fleetFixture(t)
	b.paused = map[string]bool{"miner-1": true, "miner-2": true}
	r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
		o, e := b.Call(ctx, q)
		if q.Action == "mining_status" && q.Target.Name == "miner-1" {
			o.Chain.CoreHeight, o.Chain.CoreHash = 0, "" // a receipt whose Core RPC failed
		}
		return o, e
	})
	ctx, c := deadline()
	defer c()
	if _, _, e := (&fleetLease{r: r, p: p}).check(ctx, false); e == nil || !strings.Contains(e.Error(), "checkpoint missing") {
		t.Fatal("one miner's tip stood in for the other's", e)
	}
}

func TestReadinessAsksForAgreementOnlyOnceAHeightIsKnown(t *testing.T) {
	p, r, b := migrationFixture(t)
	p.Migration.ReadinessDriven = true
	seen := map[string]int{}
	b.observe = func(q Request, o *Observation) {
		if q.Target.Role != "validator" {
			return
		}
		seen[q.Target.Name]++
		o.Problems = nil
		// The baseline sweep reaches no validator's Platform at all.
		if seen[q.Target.Name] == 1 {
			o.Problems = []string{"platform-health-unavailable"}
			o.Chain.PlatformHeight, o.Chain.DAPIHeight = 0, 0
			return
		}
		o.Chain.PlatformHeight = 500 + int64(seen[q.Target.Name])
		o.Chain.DAPIHeight = o.Chain.PlatformHeight
	}
	waits := 0
	r.Wait = func(context.Context, time.Duration) error {
		if waits++; waits > 6 {
			return context.DeadlineExceeded
		}
		return nil
	}
	ctx, c := deadline()
	defer c()
	if h, e := r.planHealth(ctx, p); e != nil || !h.Healthy {
		t.Fatal(e, h.Problems)
	}
}
