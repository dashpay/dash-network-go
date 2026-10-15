package managed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fleetBackend struct {
	mu      sync.Mutex
	base    *backend
	paused  map[string]bool
	calls   []Request
	failure string
	failed  bool
	held    int
}

func (b *fleetBackend) Call(ctx context.Context, q Request) (Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, q)
	if q.Action == "admit" && b.held > 0 && len(b.paused) == 2 {
		b.held--
		return Observation{}, &workerError{code: "migration-epoch-window", cause: errors.New("exit 1")}
	}
	if q.Action == "mining_pause" {
		b.paused[q.Target.Name] = true
	}
	if !b.failed && q.Action == b.failure {
		b.failed = true
		return Observation{}, errors.New("ambiguous " + q.Action)
	}
	if q.Action == "mining_resume" {
		delete(b.paused, q.Target.Name)
	}
	if strings.HasPrefix(q.Action, "mining_") {
		o := clone(b.base.nodes[q.Target.Name])
		o.Mining = &MiningReceipt{ServiceHash: h64("miner-service"), OperationID: q.OperationID, Paused: b.paused[q.Target.Name], Active: !b.paused[q.Target.Name], ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}
		o.Chain.CoreHeight = 2000
		o.Chain.CoreHash = h64("tip")
		return o, nil
	}
	o, e := b.base.Call(ctx, q)
	if q.CoreCheckpointHeight > 0 {
		o.Chain.CoreCheckpointHash = h64("tip")
	}
	return o, e
}
func fleetFixture(t *testing.T) (Plan, Runner, *fleetBackend) {
	p, r, b := migrationFixture(t)
	// Cover the actual requested shape: 29 validators and two unselected miners.
	for i := 14; i <= 29; i++ {
		target := clone(p.Snapshot.Fleet.Targets[0])
		target.Name = fmt.Sprintf("validator-%02d", i)
		target.InstanceID = fmt.Sprintf("i-%017x", i)
		target.Address = fmt.Sprintf("192.0.2.%d", i)
		o := clone(p.Snapshot.Nodes[p.Snapshot.Fleet.Targets[0].Name])
		o.InstanceID = target.InstanceID
		o.Chain.ProTxHash = h64(target.Name)
		o.Chain.PlatformNodeID = h64(target.Name)[:40]
		p.Snapshot.Fleet.Targets = append(p.Snapshot.Fleet.Targets, target)
		p.Snapshot.Nodes[target.Name] = o
		b.nodes[target.Name] = o
		p.Images[target.Name] = clone(p.Images[p.Snapshot.Fleet.Targets[0].Name])
	}
	p.Snapshot.Fleet.Targets[3].Architecture = "arm64"
	for _, target := range p.Snapshot.Fleet.Targets {
		p.Targets = append(p.Targets, target.Name)
	}
	for i := 1; i <= 2; i++ {
		target := Target{Name: fmt.Sprintf("miner-%d", i), Role: "core", MiningService: "dashd-generate-miner.service", InstanceID: fmt.Sprintf("i-%017x", 100+i), Address: fmt.Sprintf("192.0.2.%d", 100+i), Architecture: "amd64", Containers: map[string]string{"core": "core"}}
		o := clone(p.Snapshot.Nodes[p.Targets[0]])
		o.InstanceID = target.InstanceID
		o.Components = map[string]Container{"core": o.Components["core"]}
		o.Mining = &MiningReceipt{ServiceHash: h64("miner-service"), Active: true}
		p.Images[target.Name] = map[string]string{}
		p.Snapshot.Fleet.Targets = append(p.Snapshot.Fleet.Targets, target)
		p.Snapshot.Nodes[target.Name] = o
		b.nodes[target.Name] = o
	}
	p.Snapshot.ID = ""
	p.Snapshot.ID = hash(p.Snapshot)
	p.ID = ""
	m := *p.Migration
	m.ReadinessDriven = true
	m.FleetMiningPause = true
	m.Miners = []string{"miner-1", "miner-2"}
	m.Canaries = []string{"validator-02", "validator-04"}
	if err := p.WithMigration(m); err != nil {
		t.Fatal(err)
	}
	r.Cloud = &cloud{f: p.Snapshot.Fleet}
	remote := &fleetBackend{base: b, paused: map[string]bool{}}
	b.observe = func(q Request, o *Observation) {
		if len(remote.paused) > 0 {
			o.Chain.CoreHeight = 2000
			o.Chain.ChainLockHeight = 1999
		}
	}
	r.Remote = remote
	enroll(t, p.Snapshot, r)
	remote.calls = nil
	return p, r, remote
}
func TestFleetSinglePauseAll29BeforeResume(t *testing.T) {
	p, r, b := fleetFixture(t)
	var reports []string
	r.Progress = func(s string) { reports = append(reports, s) }
	b.held = 2 // Epoch hold after pause must not trigger a resume/re-pause cycle.
	ctx, c := deadline()
	defer c()
	record, err := r.Execute(ctx, p)
	if err != nil || record.Phase != "complete" || record.MiningPhase != "resumed" {
		t.Fatal(err, record.Phase, record.MiningPhase)
	}
	var applied []string
	pauses, resumes, renewals := 0, 0, 0
	for _, q := range b.calls {
		switch q.Action {
		case "mining_pause":
			pauses++
			if len(applied) > 0 {
				t.Fatal("pause between nodes")
			}
		case "mining_renew":
			renewals++
		case "apply":
			applied = append(applied, q.Target.Name)
			if pauses != 2 || resumes != 0 {
				t.Fatal("node outside fleet pause")
			}
		case "mining_resume":
			resumes++
			if len(applied) != 29 {
				t.Fatal("early resume", len(applied))
			}
		}
	}
	if pauses != 2 || resumes != 2 || renewals < 2 || len(applied) != 29 || !slices.Equal(applied[:2], p.Migration.Canaries) {
		t.Fatal(pauses, resumes, renewals, applied)
	}
	if !slices.Contains(reports, "both architecture canaries verified; miners remain paused") {
		t.Fatal("canary report missing")
	}
}
func TestFleetFailureRestoresMiningAndStopsFurtherWithdrawals(t *testing.T) {
	for _, failure := range []string{"mining_pause", "mining_renew", "apply"} {
		t.Run(failure, func(t *testing.T) {
			p, r, b := fleetFixture(t)
			b.failure = failure
			ctx, c := deadline()
			defer c()
			record, err := r.Execute(ctx, p)
			if err == nil || len(b.paused) > 0 || record.Phase != "interrupted" || record.MiningPhase != "resumed" {
				t.Fatal(err, record.Phase, record.MiningPhase, b.paused)
			}
			apply := 0
			for _, q := range b.calls {
				if q.Action == "apply" {
					apply++
				}
			}
			if apply > 1 {
				t.Fatal("continued after failure")
			}
			// Replaying the reviewed operation reconciles its same node and finishes.
			b.failure = ""
			record, err = r.Execute(ctx, p)
			if err != nil || record.Phase != "complete" {
				t.Fatal("resume", err, record.Phase)
			}
		})
	}
}
func TestFleetPolicyBoundAndCanariesDistinctArchitectures(t *testing.T) {
	p, _, _ := fleetFixture(t)
	for _, edit := range []func(*Migration){func(m *Migration) { m.FleetMiningPause = false }, func(m *Migration) { m.Canaries = []string{"validator-01", "validator-02"} }, func(m *Migration) { m.Miners = nil }} {
		q := clone(p)
		edit(q.Migration)
		if q.Validate() == nil {
			t.Fatal("policy not bound")
		}
		q.ID = ""
		q.ID = hash(q)
		if q.Validate() == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
func TestFleetPausedHealthStillRequiresPlatformProgress(t *testing.T) {
	p, r, b := fleetFixture(t)
	r.fleetPaused = true
	b.base.observe = func(q Request, o *Observation) {
		o.Chain.CoreHeight = 2000
		o.Chain.PlatformHeight = 2000
		o.Chain.DAPIHeight = 2000
	}
	r.Wait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	ctx, c := deadline()
	defer c()
	if _, err := r.planHealth(ctx, p); err == nil {
		t.Fatal("stalled Platform accepted")
	}
}

func TestFleetRenewerCancelsWorkOnFailure(t *testing.T) {
	p, r, b := fleetFixture(t)
	b.paused = map[string]bool{"miner-1": true, "miner-2": true}
	b.failure = "mining_renew"
	lease := &fleetLease{r: r, p: p}
	ctx, c := deadline()
	defer c()
	run, stop := lease.keep(ctx, time.Millisecond)
	select {
	case <-run.Done():
	case <-time.After(time.Second):
		t.Fatal("lease failure did not cancel rollout")
	}
	if err := stop(); err == nil || !strings.Contains(err.Error(), "renewal") {
		t.Fatal(err)
	}
}
func TestFleetLiveOwnedPauseResumesWithoutMiningCycle(t *testing.T) {
	p, r, b := fleetFixture(t)
	m := r.Store.(*memory)
	m.r.OperationID = p.ID
	m.r.Phase = "interrupted"
	m.r.MiningPhase = "paused"
	m.r.Completed = map[string]bool{}
	for _, target := range p.Snapshot.Fleet.Targets {
		m.r.Completed[target.Name] = !p.Selects(target)
	}
	b.paused = map[string]bool{"miner-1": true, "miner-2": true}
	ctx, c := deadline()
	defer c()
	record, err := r.Execute(ctx, p)
	if err != nil || record.Phase != "complete" {
		t.Fatal(err, record.Phase)
	}
	firstApply, firstResume := -1, -1
	for i, q := range b.calls {
		if q.Action == "apply" && firstApply < 0 {
			firstApply = i
		}
		if q.Action == "mining_resume" && firstResume < 0 {
			firstResume = i
		}
		if q.Action == "stage" {
			t.Fatal("re-staged during pause")
		}
	}
	if firstApply < 0 || firstResume < firstApply {
		t.Fatal("resumed mining before fleet", firstApply, firstResume)
	}
}
func TestFleetFinalResumeReplayDoesNotPauseAgain(t *testing.T) {
	p, r, b := fleetFixture(t)
	ctx, c := deadline()
	defer c()
	record, err := r.Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	m := r.Store.(*memory)
	m.r = record
	m.r.Phase = "interrupted"
	m.r.MiningPhase = "resuming"
	b.calls = nil
	record, err = r.Execute(ctx, p)
	if err != nil || record.Phase != "complete" {
		t.Fatal(err, record.Phase)
	}
	for _, q := range b.calls {
		if q.Action == "apply" || q.Action == "mining_pause" {
			t.Fatal("replayed upgrade after final resume")
		}
	}
}
func TestFleetFinalProgressStillRequired(t *testing.T) {
	p, r, b := fleetFixture(t)
	original := b.base.observe
	b.base.observe = func(q Request, o *Observation) {
		original(q, o)
		if len(b.paused) == 0 && b.failed {
			o.Chain.CoreHeight = 2000
			o.Chain.ChainLockHeight = 1999
		}
	}
	r.Progress = func(s string) {
		if s == "all selected validators verified; resuming owned miners once" {
			b.mu.Lock()
			b.failed = true
			b.mu.Unlock()
		}
	}
	r.Wait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	ctx, c := deadline()
	defer c()
	record, err := r.Execute(ctx, p)
	if err == nil || record.Phase == "complete" || record.MiningPhase != "resumed" || len(b.paused) != 0 {
		t.Fatal("final progress bypassed", err, record.Phase)
	}
}
