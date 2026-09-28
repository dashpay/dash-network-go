package managed

import (
	"context"
	"errors"
	"testing"
	"time"
)

func withMissingSeed(t *testing.T) (Snapshot, Runner, *memory, *backend) {
	s, r, m, b := setup(t)
	seed := Target{Name: "seed-1", InstanceID: "i-00000000000000099", Address: "192.0.2.99", Architecture: "amd64", Role: "seed", Containers: map[string]string{"tenderdash": "seed-tenderdash"}}
	s.Fleet.Targets = append(s.Fleet.Targets, seed)
	b.nodes[seed.Name] = Observation{InstanceID: seed.InstanceID, Error: "SSH unavailable"}
	r.Cloud = &cloud{f: s.Fleet}
	s = Observe(context.Background(), s.Fleet, b, 0)
	return s, r, m, b
}
func selectedPins(s Snapshot, target, scope, letter string) Images {
	v := pins(s, scope, letter)
	for n := range v {
		if n != target {
			v[n] = map[string]string{}
		}
	}
	return v
}
func TestDoctorRetainsMissingTargetAndVerifiesReachableNodes(t *testing.T) {
	s, r, _, _ := withMissingSeed(t)
	h, e := Doctor(context.Background(), s, r.Remote, time.Second, r.wait)
	if e != nil {
		t.Fatal(e)
	}
	if h.Healthy || len(h.Nodes) != 14 || h.Nodes["seed-1"].Status != "unknown" {
		t.Fatalf("lost partial health: %+v", h.Nodes)
	}
	for _, v := range s.Fleet.Targets[:13] {
		if !h.Nodes[v.Name].Healthy {
			t.Fatalf("%s healthy check suppressed: %+v", v.Name, h.Nodes[v.Name])
		}
	}
}
func TestSelectedEnrollmentAndUpgradeRetainAllTargetsWithoutChangingOthers(t *testing.T) {
	s, r, m, b := withMissingSeed(t)
	selected := s.Fleet.Targets[0].Name
	r.Targets = []string{selected}
	enroll(t, s, r)
	if !m.r.Enrolled[selected] || m.r.Enrolled["seed-1"] {
		t.Fatal("enrollment lost explicit scope")
	}
	p, e := BuildSelected(s, "upgrade", "dapi", "", selectedPins(s, selected, "dapi", "b"), []string{selected}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := deadline()
	defer cancel()
	result, e := r.Execute(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if result.Phase != "complete" {
		t.Fatalf("%+v", result)
	}
	for _, q := range b.calls {
		if q.Action == "observe" {
			continue
		}
		if q.Target.Name != selected {
			t.Fatalf("mutated unselected target %s: %s", q.Target.Name, q.Action)
		}
		if q.Action == "apply" && (len(q.Pins) != 1 || q.Pins["dapi"] == "") {
			t.Fatalf("component scope exceeded: %+v", q.Pins)
		}
	}
	// Later explicit enrollment must preserve the successful operation predecessor.
	fresh := Observe(ctx, s.Fleet, b, 0)
	r.Targets = []string{s.Fleet.Targets[1].Name}
	if _, e = r.Enroll(ctx, fresh); e != nil {
		t.Fatal(e)
	}
	if m.r.OperationID != p.ID {
		t.Fatal("enrollment erased predecessor")
	}
}
func TestSelectedPlanRefusesScopeExpansionAndUnavailableTarget(t *testing.T) {
	s, _, _, _ := withMissingSeed(t)
	name := s.Fleet.Targets[0].Name
	for _, targets := range [][]string{{"foreign"}, {"seed-1"}, {name, name}} {
		if _, e := BuildSelected(s, "upgrade", "dapi", "", selectedPins(s, name, "dapi", "b"), targets, time.Now()); e == nil {
			t.Fatalf("accepted targets %v", targets)
		}
	}
	if _, e := BuildSelected(s, "upgrade", "dapi", "", pins(s, "dapi", "b"), []string{name}, time.Now()); e == nil {
		t.Fatal("accepted pins outside selected node")
	}
	if !Selected("drive", "tenderdash") || Selected("dapi", "tenderdash") || Selected("gateway", "core") || ValidScope("core,core") {
		t.Fatal("component dependency scope incorrect")
	}
}
func TestSelectedValidatorWithdrawalStillRefusesUnsafeQuorum(t *testing.T) {
	s, r, _, b := setup(t)
	name := s.Fleet.Targets[0].Name
	r.Targets = []string{name}
	enroll(t, s, r)
	p, e := BuildSelected(s, "upgrade", "core", "", selectedPins(s, name, "core", "b"), []string{name}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range s.Fleet.Targets[1:5] {
		o := b.nodes[v.Name]
		o.Error = "unreachable"
		b.nodes[v.Name] = o
	}
	ctx, cancel := deadline()
	defer cancel()
	if _, e = r.Execute(ctx, p); e == nil {
		t.Fatal("unsafe withdrawal accepted")
	}
	for _, q := range b.calls {
		if q.Action == "apply" {
			t.Fatal("applied before quorum gate")
		}
	}
}
func TestSelectedLostResponseResumesSameNodeOnly(t *testing.T) {
	s, r, _, b := withMissingSeed(t)
	name := s.Fleet.Targets[0].Name
	r.Targets = []string{name}
	enroll(t, s, r)
	p, e := BuildSelected(s, "upgrade", "dapi", "", selectedPins(s, name, "dapi", "b"), []string{name}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	once := true
	b.after = func(q Request) error {
		if q.Action == "apply" && once {
			once = false
			return errors.New("lost response")
		}
		return nil
	}
	ctx, cancel := deadline()
	defer cancel()
	if _, e = r.Execute(ctx, p); e == nil {
		t.Fatal("fixture failed to interrupt")
	}
	if v, e := r.Execute(ctx, p); e != nil || v.Phase != "complete" {
		t.Fatalf("resume: %+v %v", v, e)
	}
	for _, q := range b.calls {
		if q.Action == "apply" && q.Target.Name != name {
			t.Fatal("resumed another target")
		}
	}
}
