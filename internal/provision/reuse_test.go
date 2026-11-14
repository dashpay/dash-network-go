package provision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

// Supersede is the in-memory counterpart of the journal's: the same decision,
// taken by the same NewSuccessor, without the archive.
func (s *memoryStore) Supersede(_ context.Context, p Plan, o string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != "" {
		return Record{}, errors.New("busy")
	}
	if s.record.Plan.ID != p.ID {
		next, err := NewSuccessor(p, s.record)
		if err != nil {
			return Record{}, err
		}
		s.record = next
	}
	s.owner = o
	return copyRecord(s.record), nil
}

// plainStore is a journal that cannot hand a name over.
type plainStore struct{ Store }

// destroyed provisions an IPAM fleet, terminates it and retires its addresses,
// as the status console's delete does. EC2 still lists the terminated instances.
func destroyed(t *testing.T) (Plan, *addressCloud, *memoryStore) {
	t.Helper()
	p, c, s := ipamSetup(t)
	if r, err := ipamRun(p, c, s); err != nil || r.Phase != "compute-ready" {
		t.Fatal(err)
	}
	terminateFixture(c)
	if r, err := ipamRelease(p, c, s); err != nil || r.Phase != "addresses-released" || len(c.addresses) != 0 {
		t.Fatal("fleet was not retired", err)
	}
	if err := s.record.Retired(); err != nil {
		t.Fatal("retired operation not recognised", err)
	}
	return p, c, s
}

func TestDestroyedNetworkNameIsReusedBySuccessorPlan(t *testing.T) {
	p, c, s := destroyed(t)
	ctx := context.Background()
	identity := testutil.Identity{Account: p.Network.AWS.AccountID}
	// The retired plan itself, and the same network planned afresh, stay refused.
	if _, err := ipamRun(p, c, s); !errors.Is(err, ErrRetired) || !strings.Contains(err.Error(), "allocation retired") {
		t.Fatal("retired plan provisioned again", err)
	}
	again, err := Prepare(ctx, p.Network, identity, c)
	if err != nil || again.ID != p.ID {
		t.Fatal("an unchanged network no longer plans to the same ID", err)
	}
	next, err := p.Successor(p.ID)
	if err != nil || next.ID == p.ID || next.Succeeds != p.ID || next.Key() != p.Key() || next.Validate() != nil {
		t.Fatal("invalid successor plan", err)
	}
	// Nothing about the chain changed: only the plan identity did.
	if next.Network.Metadata.Name != p.Network.Metadata.Name || next.Network.Chain != p.Network.Chain || len(next.Targets) != len(p.Targets) {
		t.Fatal("successor changed the network")
	}
	for j, target := range p.Targets {
		if next.Targets[j] != target || next.Token(target) == p.Token(target) || next.Tags(target)["dashnet:plan"] == p.Tags(target)["dashnet:plan"] {
			t.Fatal("successor would repeat the retired fleet's EC2 identity")
		}
	}
	if _, err = Execute(ctx, next, identity, c, plainStore{s}, "owner", "test", time.Millisecond, nil); err == nil || s.record.Plan.ID != p.ID {
		t.Fatal("a journal without succession handed the name over", err)
	}
	old, retiredAt := len(c.Instances), s.record.Revision
	r, err := Execute(ctx, next, identity, c, s, "owner", "test", time.Millisecond, nil)
	if err != nil || r.Phase != "compute-ready" || r.Plan.ID != next.ID || len(c.Instances) != old+len(p.Targets) || len(c.addresses) != len(p.Targets) {
		t.Fatal("successor did not provision past the terminated predecessor", err)
	}
	if r.Predecessor == nil || r.Predecessor.PlanID != p.ID || r.Predecessor.Revision != retiredAt || r.Predecessor.Archive != p.ArchiveKey(retiredAt) || len(r.Predecessor.Instances) != len(p.Targets) || r.Bootstrap != nil || r.Deployment != nil || r.Runtime != nil || r.Upgrade != nil {
		t.Fatal("successor operation is not fresh or lost its predecessor")
	}
	for _, target := range next.Targets {
		n := r.Nodes[target.Name]
		for _, id := range r.Predecessor.Instances {
			if n.InstanceID == id {
				t.Fatal("successor adopted a retired instance")
			}
		}
	}
	// A resume, and the later stages, still see the terminated predecessor.
	if _, err = Execute(ctx, next, identity, c, s, "owner", "test", time.Millisecond, nil); err != nil || len(c.Instances) != old+len(p.Targets) {
		t.Fatal("successor resume relaunched or failed", err)
	}
	if live, err := RunningTargets(ctx, next, s.record, c); err != nil || len(live) != len(next.Targets) {
		t.Fatal("bootstrap cannot bind the successor's targets", err)
	}
	// The successor can itself be destroyed and succeeded.
	for j := range c.Instances {
		c.Instances[j].State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
	}
	for j := range c.addresses {
		c.addresses[j].AssociationId, c.addresses[j].NetworkInterfaceId, c.addresses[j].InstanceId = nil, nil, nil
	}
	if r, err = ipamRelease(next, c, s); err != nil || r.Phase != "addresses-released" || r.Retired() != nil {
		t.Fatal("successor was not retired", err)
	}
	third, err := next.Successor(next.ID)
	if err != nil || third.ID == next.ID || third.ID == p.ID {
		t.Fatal("second successor repeats an earlier plan", err)
	}
	if _, err = NewSuccessor(third, s.record); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessorNeverAdoptsOrIgnoresAnythingButItsTerminatedPredecessor(t *testing.T) {
	for _, change := range []string{"shutting-down", "stopped", "running", "unjournaled", "other-plan", "foreign-node", "live-foreign"} {
		t.Run(change, func(t *testing.T) {
			p, c, s := destroyed(t)
			next, err := p.Successor(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			retag := func(i *types.Instance, key, value string) {
				tags := []types.Tag{}
				for _, tag := range i.Tags {
					if aws.ToString(tag.Key) == key {
						tag.Value = aws.String(value)
					}
					tags = append(tags, tag)
				}
				i.Tags = tags
			}
			switch change {
			case "shutting-down":
				c.Instances[0].State = &types.InstanceState{Name: types.InstanceStateNameShuttingDown}
			case "stopped":
				c.Instances[0].State = &types.InstanceState{Name: types.InstanceStateNameStopped}
			case "running":
				c.Instances[0].State = &types.InstanceState{Name: types.InstanceStateNameRunning}
			case "unjournaled":
				// Terminated and tagged like the predecessor, but not an instance it recorded.
				extra := c.Instances[0]
				extra.InstanceId = aws.String("i-0000beef")
				c.Instances = append(c.Instances, extra)
			case "other-plan":
				retag(&c.Instances[0], "dashnet:plan", strings.Repeat("a", 64))
			case "foreign-node":
				// One of the predecessor's instances is exempt; a renamed live one is not.
				c.Instances[0].State = &types.InstanceState{Name: types.InstanceStateNameRunning}
				retag(&c.Instances[0], "dashnet:node", "legacy-1")
			case "live-foreign":
				c.Instances = append(c.Instances, types.Instance{InstanceId: aws.String("i-0000f00d"), State: &types.InstanceState{Name: types.InstanceStateNameRunning}, Tags: []types.Tag{{Key: aws.String(p.Network.AWS.NetworkTagKey), Value: aws.String(p.Network.Metadata.Name)}}})
			}
			before := len(c.Instances)
			r, err := ipamRun(next, c, s)
			if err == nil || len(c.Instances) != before || len(c.Requests) != len(p.Targets) || len(c.addresses) != 0 {
				t.Fatal("successor launched beside an unexplained instance", err)
			}
			if r.Phase != "interrupted" || s.record.Plan.ID != next.ID || s.owner != "" {
				t.Fatal("refusal did not leave an inspectable, unclaimed operation", r.Phase)
			}
		})
	}
}

func TestOnlyARetiredOperationHasASuccessor(t *testing.T) {
	p, c, s := ipamSetup(t)
	if _, err := ipamRun(p, c, s); err != nil {
		t.Fatal(err)
	}
	next, err := p.Successor(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A running network: every phase short of released addresses is refused.
	for _, phase := range []string{"pending", "provisioning", "interrupted", "compute-ready"} {
		r := copyRecord(s.record)
		r.Phase = phase
		if phase == "pending" {
			for name := range r.Nodes {
				r.Nodes[name] = NodeProgress{Phase: "pending", EC2State: "unknown"}
			}
		}
		if err := r.Validate(p); err != nil {
			t.Fatal(phase, err)
		}
		if r.Retired() == nil {
			t.Fatal("unretired operation accepted in phase", phase)
		}
		if _, err := NewSuccessor(next, r); err == nil {
			t.Fatal("unretired operation succeeded in phase", phase)
		}
	}
	if _, err := ipamRun(next, c, s); err == nil || s.record.Plan.ID != p.ID || len(c.Requests) != len(p.Targets) {
		t.Fatal("a live network's name was handed over", err)
	}
	terminateFixture(c)
	if _, err := ipamRelease(p, c, s); err != nil {
		t.Fatal(err)
	}
	retired := copyRecord(s.record)
	if _, err := NewSuccessor(next, retired); err != nil {
		t.Fatal(err)
	}
	// A plan naming another predecessor, or none, is not this operation's successor.
	other, err := p.Successor(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []Plan{p, other} {
		if _, err := NewSuccessor(wrong, retired); err == nil {
			t.Fatal("operation succeeded by a plan that does not name it")
		}
	}
	// Retirement is exactly the proof ReleaseAddresses leaves, nothing weaker.
	for _, change := range []string{"address", "instance", "phase"} {
		r := copyRecord(retired)
		name := p.Targets[0].Name
		n := r.Nodes[name]
		switch change {
		case "address":
			n.Address.Phase = "releasing"
		case "instance":
			n.InstanceID = ""
		case "phase":
			r.Phase = "compute-ready"
		}
		r.Nodes[name] = n
		if r.Retired() == nil {
			t.Fatal("weaker retirement proof accepted:", change)
		}
	}
	// A successor's own journal must keep, and match, its predecessor.
	fresh, err := NewSuccessor(next, retired)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"lost", "plan", "archive", "revision", "instances", "unsorted"} {
		r := copyRecord(fresh)
		switch change {
		case "lost":
			r.Predecessor = nil
		case "plan":
			r.Predecessor.PlanID = strings.Repeat("a", 64)
		case "archive":
			r.Predecessor.Archive = p.Key()
		case "revision":
			// The archive key names the revision that was replaced.
			r.Predecessor.Revision++
		case "instances":
			r.Predecessor.Instances = nil
		case "unsorted":
			r.Predecessor.Instances = []string{"i-2", "i-1"}
		}
		if r.Validate(next) == nil {
			t.Fatal("invalid predecessor accepted:", change)
		}
	}
	first := NewRecord(p)
	first.Predecessor = fresh.Predecessor
	if first.Validate(p) == nil {
		t.Fatal("a first plan's operation claimed a predecessor")
	}
}

func TestPrivatePlanIsNeverRetired(t *testing.T) {
	// No IPAM addresses, so no address retirement, so no proof the fleet is gone.
	p, c, s := setup(t)
	if _, err := run(context.Background(), p, c, s, "owner"); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"compute-ready", "addresses-released"} {
		r := copyRecord(s.record)
		r.Phase = phase
		if r.Retired() == nil {
			t.Fatal("a plan without IPAM addresses was treated as retired in phase", phase)
		}
	}
	next, err := p.Successor(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run(context.Background(), next, c, s, "owner"); err == nil || s.record.Plan.ID != p.ID {
		t.Fatal("private network name handed over without proof", err)
	}
	if _, err = p.Successor("not-a-plan"); err == nil {
		t.Fatal("invalid predecessor ID accepted")
	}
	tampered := next
	tampered.Succeeds = strings.Repeat("b", 64)
	if tampered.Validate() == nil {
		t.Fatal("plan ID does not cover its predecessor")
	}
}

func TestFirstPlansAndOperationsAreSerializedAsBefore(t *testing.T) {
	// Existing plan IDs are digests of this JSON and existing journals are
	// decoded strictly: a first plan and its operation must not gain a field.
	p, _, _ := setup(t)
	plan, err := json.Marshal(p)
	if err != nil || strings.Contains(string(plan), "succeeds") {
		t.Fatal("a first plan serializes a succession", err)
	}
	record, err := json.Marshal(NewRecord(p))
	if err != nil || strings.Contains(string(record), "predecessor") || strings.Contains(string(record), "succeeds") {
		t.Fatal("a first operation serializes a predecessor", err)
	}
	unsalted := p
	unsalted.ID = ""
	if digest(unsalted) != p.ID {
		t.Fatal("a first plan's ID is no longer the digest of its original fields")
	}
	next, err := p.Successor(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = json.Marshal(next)
	if err != nil || !strings.Contains(string(plan), `"succeeds":"`+p.ID+`"`) {
		t.Fatal("a successor plan does not state what it succeeds", err)
	}
}
