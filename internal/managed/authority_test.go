package managed

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAuthorityIgnoresTargetsAndDisplayMetadata(t *testing.T) {
	s, _, _, _ := setup(t)
	f := clone(s.Fleet)
	f.Targets = f.Targets[:3]
	f.Metadata.DisplayName = "Renamed"
	f.Metadata.Description = "edited from the console"
	if f.Authority() != s.Fleet.Authority() || f.ID() == s.Fleet.ID() {
		t.Fatal("authority must ignore targets/display metadata while ID binds them")
	}
	f.StateTable = "other-table"
	if f.Authority() == s.Fleet.Authority() {
		t.Fatal("authority must bind the journal scope")
	}
}

// A discovered second seed joins an already enrolled network without
// orphaning the journal or re-enrolling existing targets.
func TestEnrolledFleetGainsTargetAndEnrollsOnlyNewcomer(t *testing.T) {
	s, r, m, b := setup(t)
	enroll(t, s, r)
	grown := clone(s.Fleet)
	seed := Target{Name: "seed-2", InstanceID: "i-000000000000000a2", Address: "192.0.2.102", Architecture: "amd64", Role: "seed", Containers: map[string]string{"tenderdash": "tenderdash"}}
	grown.Targets = append(grown.Targets, seed)
	o := clone(b.nodes["validator-01"])
	o.InstanceID = seed.InstanceID
	o.Components = map[string]Container{"tenderdash": o.Components["tenderdash"]}
	b.nodes[seed.Name] = o
	r.Cloud = &cloud{f: grown}
	ctx, cancel := deadline()
	defer cancel()
	fresh := Observe(ctx, grown, b, 0)
	if e := fresh.Complete(); e != nil {
		t.Fatal(e)
	}
	b.calls = nil
	record, e := r.Enroll(ctx, fresh)
	if e != nil {
		t.Fatal(e)
	}
	enrolls := 0
	for _, q := range b.calls {
		if q.Action == "enroll" {
			enrolls++
			if q.Target.Name != "seed-2" {
				t.Fatalf("re-enrolled %s", q.Target.Name)
			}
		}
		if q.FleetID != grown.Authority() {
			t.Fatal("request not bound to network authority")
		}
	}
	if enrolls != 1 || !record.IsEnrolled(seed) || record.SnapshotID != fresh.ID || m.owner != "" {
		t.Fatalf("growth enrollment: %d %+v", enrolls, record)
	}
	p, e := BuildSelected(fresh, "upgrade", "tenderdash", record.OperationID, selectedPins(fresh, "seed-2", "tenderdash", "c"), []string{"seed-2"}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Execute(ctx, p); e != nil {
		t.Fatal(e)
	}
}

func TestReplacedInstanceUnderSameNameIsNotEnrolled(t *testing.T) {
	s, r, _, b := setup(t)
	enroll(t, s, r)
	moved := clone(s.Fleet)
	moved.Targets[0].InstanceID = "i-0000000000000ffff"
	o := b.nodes[moved.Targets[0].Name]
	o.InstanceID = moved.Targets[0].InstanceID
	b.nodes[moved.Targets[0].Name] = o
	r.Cloud = &cloud{f: moved}
	ctx, cancel := deadline()
	defer cancel()
	fresh := Observe(context.Background(), moved, b, 0)
	name := moved.Targets[0].Name
	p, e := BuildSelected(fresh, "upgrade", "dapi", "", selectedPins(fresh, name, "dapi", "d"), []string{name}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Execute(ctx, p); e == nil || !strings.Contains(e.Error(), "enrollment") {
		t.Fatalf("replacement inherited enrollment: %v", e)
	}
	r.Targets = []string{name}
	record, e := r.Enroll(ctx, fresh)
	if e != nil || !record.IsEnrolled(moved.Targets[0]) {
		t.Fatalf("replacement enrollment: %v %+v", e, record)
	}
}
