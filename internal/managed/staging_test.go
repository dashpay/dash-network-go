package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stagingBackend func(context.Context, Request) (Observation, error)

func (b stagingBackend) Call(ctx context.Context, q Request) (Observation, error) { return b(ctx, q) }

// Use legacy Testnet masternodes, including more hosts than the staging bound.
func stagingPlan(t *testing.T, count int, operation string) (Plan, Runner, *memory, *backend) {
	t.Helper()
	s, r, m, b := setup(t)
	f := s.Fleet
	f.Metadata.Name, f.ChainType, f.CoreNetwork = "testnet", "testnet", "test"
	f.Targets = nil
	base := s.Nodes[s.Fleet.Targets[0].Name]
	b.nodes = map[string]Observation{}
	for i := 1; i <= count; i++ {
		name := fmt.Sprintf("masternode-%03d", i)
		target := Target{Name: name, InstanceID: fmt.Sprintf("i-%017x", i), Address: fmt.Sprintf("192.0.2.%d", i), Architecture: "amd64", Role: "masternode", Containers: map[string]string{"core": name + "-core"}}
		if i%2 == 0 {
			target.Architecture = "arm64"
		}
		o := clone(base)
		o.InstanceID, o.Chain.CoreNetwork = target.InstanceID, "test"
		o.Components = map[string]Container{"core": o.Components["core"]}
		f.Targets = append(f.Targets, target)
		b.nodes[name] = o
	}
	r.Cloud = &cloud{f: f}
	s = Observe(context.Background(), f, b, 0)
	letter := "b"
	if operation == "deploy" {
		letter = "a"
	}
	images := pins(s, "core", letter)
	if operation == "upgrade" {
		for _, target := range f.Targets {
			if target.Architecture == "arm64" {
				images[target.Name]["core"] = "docker.io/dashpay/core@sha256:" + strings.Repeat("c", 64)
			}
		}
	}
	p, err := Build(s, operation, "core", "", images, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p, r, m, b
}

func TestManagedParallelStagingSelectionAndBound(t *testing.T) {
	p, r, _, _ := stagingPlan(t, 2*stagingConcurrency+3, "upgrade")
	var selected []string
	for _, target := range p.Snapshot.Fleet.Targets[:len(p.Snapshot.Fleet.Targets)-1] {
		selected = append(selected, target.Name)
	}
	ignored := p.Snapshot.Fleet.Targets[len(p.Snapshot.Fleet.Targets)-1].Name
	p.Images[ignored] = map[string]string{}
	var err error
	p, err = BuildSelected(p.Snapshot, p.Operation, p.Scope, "", p.Images, selected, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := deadline()
	defer cancel()
	started := make(chan struct{}, len(selected))
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var mu sync.Mutex
	active, peak := 0, 0
	seen := map[string]int{}
	r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
		if q.Action != "stage" || q.Target.Name == ignored || q.OperationID != p.ID || q.Operation != p.Operation || q.FleetID != p.Snapshot.Fleet.Authority() || q.Expected == nil || hash(*q.Expected) != hash(p.Snapshot.Nodes[q.Target.Name]) || hash(q.Pins) != hash(p.Images[q.Target.Name]) {
			t.Error("staging exceeded reviewed intent")
		}
		mu.Lock()
		active++
		peak = max(peak, active)
		seen[q.Target.Name]++
		mu.Unlock()
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		active--
		mu.Unlock()
		return Observation{}, ctx.Err()
	})
	// Deliberately non-thread-safe: progress must run on the caller, not workers.
	progress := 0
	r.Progress = func(string) { progress++ }
	done := make(chan error, 1)
	go func() { done <- r.stage(ctx, p) }()
	for range stagingConcurrency {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("staging did not overlap", ctx.Err())
		}
	}
	select {
	case <-started:
		t.Fatal("staging exceeded the concurrency bound")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if active != 0 || peak != stagingConcurrency || len(seen) != len(selected) || progress != len(selected) {
		t.Fatalf("active=%d peak=%d staged=%d progress=%d", active, peak, len(seen), progress)
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("%s staged %d times", name, count)
		}
	}
}

func TestManagedStagingCancellationDrainsBeforeReturn(t *testing.T) {
	p, r, m, b := stagingPlan(t, stagingConcurrency+3, "upgrade")
	enroll(t, p.Snapshot, r)
	ctx, cancel := deadline()
	defer cancel()
	started := make(chan struct{}, len(p.Images))
	cancelled := make(chan struct{}, len(p.Images))
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
		if q.Action != "stage" {
			if q.Action == "apply" {
				t.Error("replaced a container after cancelled staging")
			}
			return b.Call(ctx, q)
		}
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-release // cancellation is not proof that an in-flight call has exited
		return Observation{}, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { _, err := r.Execute(ctx, p); done <- err }()
	for range stagingConcurrency {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("workers did not start")
		}
	}
	cancel()
	for range stagingConcurrency {
		<-cancelled
	}
	if m.owner != r.Owner || m.r.Phase != "staging" {
		t.Fatal("released or changed the journal with staging still in flight")
	}
	select {
	case <-done:
		t.Fatal("returned with staging still in flight")
	default:
	}
	for range stagingConcurrency {
		release <- struct{}{}
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation", err)
	}
	if m.owner != "" || m.r.Phase != "interrupted" {
		t.Fatal("cancelled staging did not release its recoverable journal")
	}
	if calls.Load() != stagingConcurrency {
		t.Fatal("started another host after cancellation")
	}
	if err := r.stage(ctx, p); !errors.Is(err, context.Canceled) || calls.Load() != stagingConcurrency {
		t.Fatal("already-cancelled staging touched a host", err)
	}
}

func TestManagedStagingFailureRetainsEveryHostAndResumes(t *testing.T) {
	for _, operation := range []string{"upgrade", "deploy"} {
		t.Run(operation, func(t *testing.T) {
			p, r, m, b := stagingPlan(t, 5, operation)
			enroll(t, p.Snapshot, r)
			bad1, bad2 := p.Snapshot.Fleet.Targets[1].Name, p.Snapshot.Fleet.Targets[3].Name
			b.after = func(q Request) error {
				if q.Action == "stage" && (q.Target.Name == bad1 || q.Target.Name == bad2) {
					return errors.New("registry unavailable")
				}
				return nil
			}
			ctx, cancel := deadline()
			defer cancel()
			_, err := r.Execute(ctx, p)
			if err == nil || !strings.Contains(err.Error(), bad1+": registry unavailable") || !strings.Contains(err.Error(), bad2+": registry unavailable") {
				t.Fatal("lost host staging failures", err)
			}
			staged := 0
			for _, q := range b.calls {
				if q.Action == "apply" {
					t.Fatal("replaced a container after staging failure")
				}
				if q.Action == "stage" {
					staged++
				}
			}
			if staged != len(p.Images) || m.r.Phase != "interrupted" || m.r.Current != "" || m.r.OperationID != p.ID || m.owner != "" {
				t.Fatal("staging failed without a recoverable journal", m.r, staged)
			}
			b.after = nil
			b.calls = nil
			result, err := r.Execute(ctx, p)
			if err != nil || result.Phase != "complete" {
				t.Fatal("same-plan resume failed", err)
			}
			staged = 0
			for _, q := range b.calls {
				if q.Action == "stage" {
					staged++
				}
				if q.Action == "apply" && staged != len(p.Images) {
					t.Fatal("replacement began before every staging call finished")
				}
			}
		})
	}
}
