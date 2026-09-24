package managed

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/node"
)

type Runner struct {
	Identity inventory.STS
	Cloud    inventory.EC2
	Store    Store
	Remote   Backend
	Owner    string
	Window   time.Duration
	Progress func(string)
	Wait     func(context.Context, time.Duration) error
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (r Runner) wait(ctx context.Context, d time.Duration) error {
	if r.Wait != nil {
		return r.Wait(ctx, d)
	}
	return sleep(ctx, d)
}
func (r Runner) check(ctx context.Context, f Fleet) error {
	if r.Remote == nil || r.Store == nil || r.Owner == "" {
		return errors.New("authenticated backend, store and owner required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("operation deadline required")
	}
	return VerifyCloud(ctx, f, r.Identity, r.Cloud)
}
func (r Runner) report(s string) {
	if r.Progress != nil {
		r.Progress(s)
	}
}
func (r Runner) health(ctx context.Context, s Snapshot) (Health, error) {
	w := r.Window
	if w == 0 {
		w = 90 * time.Second
	}
	return Doctor(ctx, s, r.Remote, w, r.wait)
}
func (r Runner) Enroll(ctx context.Context, s Snapshot) (record Record, err error) {
	if err = s.Complete(); err != nil {
		return
	}
	f := s.Fleet
	if err = r.check(ctx, f); err != nil {
		return
	}
	record, err = r.Store.Acquire(ctx, f, s, r.Owner)
	if err != nil {
		return
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if e := r.Store.Release(c, f, r.Owner); e != nil {
			err = errors.Join(err, e)
		}
	}()
	if record.SnapshotID != s.ID {
		return record, errors.New("resume enrollment with its original snapshot")
	}
	if record.OperationID != "" {
		return record, errors.New("already in managed operation; enrollment is not redeployment")
	}
	for _, t := range f.Targets {
		if record.Enrolled[t.Name] {
			continue
		}
		q := request(f, t, "enroll")
		v := s.Nodes[t.Name]
		q.Expected = &v
		if _, err = r.Remote.Call(ctx, q); err != nil {
			return
		}
		record.Enrolled[t.Name] = true
		record.Revision++
		if err = r.Store.Save(ctx, f, record, r.Owner); err != nil {
			return
		}
		r.report("enrolled " + t.Name + "; services unchanged")
	}
	record.Phase = "enrolled"
	record.Revision++
	err = r.Store.Save(ctx, f, record, r.Owner)
	return
}
func (r Runner) Execute(ctx context.Context, p Plan) (record Record, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	f := p.Snapshot.Fleet
	if err = r.check(ctx, f); err != nil {
		return
	}
	// Refuse implicit enrollment when executing a plan.
	prior, _, err := r.Store.Read(ctx, f)
	if err != nil {
		return
	}
	for _, t := range f.Targets {
		if !prior.Enrolled[t.Name] {
			return record, errors.New("finish explicit enrollment first")
		}
	}
	record, err = r.Store.Acquire(ctx, f, p.Snapshot, r.Owner)
	if err != nil {
		return
	}
	mutated := false
	save := func() error { record.Revision++; return r.Store.Save(ctx, f, record, r.Owner) }
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err != nil && mutated {
			record.Phase = "interrupted"
			record.LastError = node.SafeError(err)
			record.Revision++
			if e := r.Store.Save(c, f, record, r.Owner); e != nil {
				err = errors.Join(err, e)
			}
		}
		if e := r.Store.Release(c, f, r.Owner); e != nil {
			err = errors.Join(err, e)
		}
	}()
	resuming := record.OperationID == p.ID
	if !resuming {
		if record.OperationID != p.PreviousID || (record.Phase != "enrolled" && record.Phase != "complete") {
			return record, errors.New("stale plan or a different unfinished operation")
		}
		current := Observe(ctx, f, r.Remote, 0)
		for _, t := range f.Targets {
			selected := []string{}
			if p.Operation == "deploy" {
				for c := range p.Images[t.Name] {
					selected = append(selected, c)
				}
			}
			if err = Preserved(p.Snapshot.Nodes[t.Name], current.Nodes[t.Name], selected); err != nil {
				return record, fmt.Errorf("%s preflight: %w", t.Name, err)
			}
			// A stopped workload may change running/start counters, not ownership.
			// Reject same-name replacements before staging anything on any host.
			for _, c := range selected {
				before, now := p.Snapshot.Nodes[t.Name].Components[c], current.Nodes[t.Name].Components[c]
				if before.ID != now.ID || before.ImageID != now.ImageID {
					return record, fmt.Errorf("%s/%s: deployment source was replaced", t.Name, c)
				}
			}
		}
		if p.Operation == "upgrade" {
			h, e := r.health(ctx, p.Snapshot)
			if e != nil {
				return record, e
			}
			if !h.Healthy {
				return record, errors.New("upgrade requires whole managed fleet healthy: " + strings.Join(h.Problems, "; "))
			}
		}
		record.OperationID = p.ID
		record.Phase = "staging"
		record.Completed = map[string]bool{}
		record.LastError = ""
		for _, t := range f.Targets {
			record.Completed[t.Name] = len(p.Images[t.Name]) == 0
		}
		mutated = true
		if err = save(); err != nil {
			return
		}
	}
	if record.Phase == "complete" {
		h, e := r.health(ctx, p.Snapshot)
		if e != nil {
			return record, e
		}
		if !h.Healthy {
			return record, errors.New("completed operation is currently unhealthy")
		}
		return record, nil
	}
	mutated = true
	r.report("managed-staging")
	// Staging is bounded and never changes a container. Keep every target visible.
	for _, t := range f.Targets {
		if len(p.Images[t.Name]) == 0 {
			continue
		}
		q := request(f, t, "stage")
		v := p.Snapshot.Nodes[t.Name]
		q.Expected = &v
		q.Pins = p.Images[t.Name]
		q.Operation = p.Operation
		q.OperationID = p.ID
		if _, err = r.Remote.Call(ctx, q); err != nil {
			return
		}
	}
	targets := slices.Clone(f.Targets)
	if current := record.Current; current != "" {
		sort.SliceStable(targets, func(i, j int) bool { return targets[i].Name == current && targets[j].Name != current })
	}
	for _, t := range targets {
		if record.Completed[t.Name] {
			continue
		}
		if err = VerifyCloud(ctx, f, r.Identity, r.Cloud); err != nil {
			return
		}
		// Resume only the journaled target; it may be between stop/remove/create.
		if p.Operation == "upgrade" && record.Current == "" {
			h, e := r.health(ctx, p.Snapshot)
			if e != nil {
				return record, e
			}
			if !h.Healthy {
				return record, errors.New("fleet unhealthy before next withdrawal")
			}
			if err = CanWithdraw(h.Snapshot, t); err != nil {
				return
			}
		}
		record.Current = t.Name
		record.Phase = "applying"
		if err = save(); err != nil {
			return
		}
		r.report("managed-applying")
		q := request(f, t, "apply")
		v := p.Snapshot.Nodes[t.Name]
		q.Expected = &v
		q.Pins = maps.Clone(p.Images[t.Name])
		q.Operation = p.Operation
		q.OperationID = p.ID
		if _, err = r.Remote.Call(ctx, q); err != nil {
			return
		}
		record.Phase = "verifying"
		if err = save(); err != nil {
			return
		}
		if p.Operation == "upgrade" {
			if err = r.verify(ctx, p, record); err != nil {
				return
			}
		}
		record.Completed[t.Name] = true
		record.Current = ""
		if err = save(); err != nil {
			return
		}
		r.report("managed target " + t.Name + " applied")
	}
	if err = r.verify(ctx, p, record); err != nil {
		return
	}
	record.Phase = "complete"
	record.LastError = ""
	err = save()
	return
}
func (r Runner) verify(ctx context.Context, p Plan, record Record) error {
	check, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	r.report("managed-verifying")
	for {
		h, e := r.health(check, p.Snapshot)
		if e != nil {
			return e
		}
		if h.Healthy {
			for _, t := range p.Snapshot.Fleet.Targets {
				selected := []string{}
				if record.Completed[t.Name] || record.Current == t.Name {
					for c := range p.Images[t.Name] {
						selected = append(selected, c)
					}
				}
				current := h.Snapshot.Nodes[t.Name]
				if e = Preserved(p.Snapshot.Nodes[t.Name], current, selected); e != nil {
					return fmt.Errorf("%s: %w", t.Name, e)
				}
				for _, c := range selected {
					pin := p.Images[t.Name][c]
					found := false
					for _, d := range current.Components[c].Digests {
						_, got, ok := strings.Cut(d, "@")
						_, want, _ := strings.Cut(pin, "@")
						if ok && got == want {
							found = true
						}
					}
					if !found {
						return fmt.Errorf("%s/%s: running artifact not verified", t.Name, c)
					}
				}
			}
			return nil
		}
		r.report("managed health gate waiting: " + strings.Join(h.Problems, "; "))
		if e = r.wait(check, 15*time.Second); e != nil {
			return e
		}
	}
}
