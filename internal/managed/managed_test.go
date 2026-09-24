package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/journal"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/testutil"
	"strings"
	"sync"
	"testing"
	"time"
)

func clone[T any](v T) T  { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }
func h64(s string) string { return hash(s) }

type memory struct {
	r     Record
	owner string
}

func (m *memory) Read(_ context.Context, _ Fleet) (Record, string, error) {
	if m.r.FleetID == "" {
		return Record{}, "", journal.ErrNotFound
	}
	return clone(m.r), m.owner, nil
}
func (m *memory) Acquire(_ context.Context, f Fleet, s Snapshot, o string) (Record, error) {
	if m.owner != "" {
		return Record{}, errors.New("claimed")
	}
	m.owner = o
	if m.r.FleetID == "" {
		m.r = Record{FleetID: f.ID(), SnapshotID: s.ID, Phase: "enrolling", Enrolled: map[string]bool{}}
		for _, t := range f.Targets {
			m.r.Enrolled[t.Name] = false
		}
	}
	return clone(m.r), nil
}
func (m *memory) Save(_ context.Context, f Fleet, r Record, o string) error {
	if m.owner != o || r.Revision != m.r.Revision+1 {
		return errors.New("stale write")
	}
	if e := r.Validate(f); e != nil {
		return e
	}
	m.r = clone(r)
	return nil
}
func (m *memory) Release(_ context.Context, _ Fleet, o string) error {
	if m.owner != o {
		return errors.New("wrong owner")
	}
	m.owner = ""
	return nil
}

type cloud struct {
	f       Fleet
	foreign bool
}

func (c *cloud) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	o := &ec2.DescribeInstancesOutput{}
	r := ec2types.Reservation{}
	for _, t := range c.f.Targets {
		tag := c.f.Metadata.Name
		if c.foreign {
			tag = "other"
		}
		r.Instances = append(r.Instances, ec2types.Instance{InstanceId: aws.String(t.InstanceID), PublicIpAddress: aws.String(t.Address), Architecture: ec2types.ArchitectureValuesX8664, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}, Tags: []ec2types.Tag{{Key: aws.String(c.f.NetworkTagKey), Value: aws.String(tag)}}})
	}
	o.Reservations = []ec2types.Reservation{r}
	return clone(o), nil
}

type backend struct {
	mu       sync.Mutex
	nodes    map[string]Observation
	calls    []Request
	fail     string
	failOnce bool
	after    func(Request) error
}

func (b *backend) Call(ctx context.Context, q Request) (Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return Observation{}, e
	}
	b.calls = append(b.calls, q)
	o := clone(b.nodes[q.Target.Name])
	if q.Action == b.fail && b.failOnce {
		b.failOnce = false
		return Observation{}, errors.New("lost-before-apply")
	}
	if q.Action == "observe" {
		o.Chain.CoreHeight++
		o.Chain.ChainLockHeight++
		o.Chain.PlatformHeight++
		o.Chain.DAPIHeight++
		if q.ReferenceHeight > 0 {
			o.Chain.PlatformHash = h64("same-block")
		}
		b.nodes[q.Target.Name] = o
	}
	if q.Action == "apply" {
		for c, pin := range q.Pins {
			v := o.Components[c]
			if v.Image != pin {
				v.ID = h64(q.Target.Name + c + pin)
				v.ImageID = "sha256:" + h64(pin)
				v.Image = pin
				v.Digests = []string{pin}
			}
			v.Running = true
			o.Components[c] = v
		}
		b.nodes[q.Target.Name] = o
	}
	if b.after != nil {
		if e := b.after(q); e != nil {
			return Observation{}, e
		}
	}
	return clone(o), nil
}
func setup(t *testing.T) (Snapshot, Runner, *memory, *backend) {
	t.Helper()
	f := Fleet{APIVersion: spec.Version, Kind: "ExistingNetwork", Metadata: spec.Metadata{Name: "devnet-example", DisplayName: "Example", Visibility: "private"}, ChainType: "devnet", CoreNetwork: "devnet-example", AccountID: "123456789012", Region: "us-west-2", NetworkTagKey: "DashNetwork", StateTable: "managed-state", Access: bootstrap.Access{User: "ubuntu", Port: 22, Address: "public"}}
	powers := map[string]int64{}
	for i := 1; i <= 12; i++ {
		powers[h64(fmt.Sprint(i))] = 100
	}
	nodes := map[string]Observation{}
	for i := 1; i <= 13; i++ {
		name := fmt.Sprintf("validator-%02d", i)
		target := Target{Name: name, InstanceID: fmt.Sprintf("i-%017x", i), Address: fmt.Sprintf("192.0.2.%d", i), Architecture: "amd64", Role: "validator", Containers: map[string]string{}}
		o := Observation{InstanceID: target.InstanceID, At: time.Now().UTC(), Components: map[string]Container{}, Companions: map[string]Container{}, FilesHash: h64("files"), Problems: []string{}, Chain: Chain{CoreNetwork: f.CoreNetwork, CoreGenesis: h64("genesis"), CoreHeight: 100, CoreSynced: true, ChainLockHeight: 99, MasternodeState: "READY", ProTxHash: h64(fmt.Sprint(i)), PlatformChainID: "dash-devnet-example", PlatformHeight: 100, PlatformProtocol: 14, PlatformNodeID: h64(name)[:40], VotingPower: clone(powers), DAPIHeight: 100, DAPIHealthy: true}}
		for _, c := range Components {
			target.Containers[c] = name + "-" + c
			pin := "docker.io/dashpay/" + c + "@sha256:" + strings.Repeat("a", 64)
			o.Components[c] = Container{ID: h64(name + c), ImageID: "sha256:" + h64(pin), Image: pin, Digests: []string{pin}, ConfigHash: h64("config" + c), Running: true, StartedAt: "2026-09-24T00:00:00Z"}
		}
		f.Targets = append(f.Targets, target)
		nodes[name] = o
	}
	b := &backend{nodes: nodes}
	s := Observe(context.Background(), f, b, 0)
	if e := s.Complete(); e != nil {
		t.Fatal(e)
	}
	m := &memory{}
	r := Runner{Identity: testutil.Identity{Account: f.AccountID}, Cloud: &cloud{f: f}, Store: m, Remote: b, Owner: "runner", Window: time.Second, Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	return s, r, m, b
}
func pins(s Snapshot, scope, letter string) Images {
	out := Images{}
	for _, t := range s.Fleet.Targets {
		out[t.Name] = map[string]string{}
		for c := range t.Containers {
			if Selected(scope, c) {
				out[t.Name][c] = "docker.io/dashpay/" + c + "@sha256:" + strings.Repeat(letter, 64)
			}
		}
	}
	return out
}
func deadline() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
func enroll(t *testing.T, s Snapshot, r Runner) {
	t.Helper()
	ctx, c := deadline()
	defer c()
	if _, e := r.Enroll(ctx, s); e != nil {
		t.Fatal(e)
	}
}
func TestManagedImportEnrollmentAndSerialUpgrade(t *testing.T) {
	s, r, m, b := setup(t)
	if len(b.calls) != 13 {
		t.Fatal("import lost targets")
	}
	for _, q := range b.calls {
		if q.Action != "observe" {
			t.Fatal("import mutated")
		}
	}
	enroll(t, s, r)
	p, e := Build(s, "upgrade", "platform", "", pins(s, "platform", "b"), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	last := ""
	b.after = func(q Request) error {
		if q.Action == "apply" {
			if m.r.Current != q.Target.Name {
				t.Error("apply preceded journal intent")
			}
			if last != "" && !m.r.Completed[last] {
				t.Error("withdrew next node before gate")
			}
			last = q.Target.Name
		}
		return nil
	}
	ctx, c := deadline()
	defer c()
	result, e := r.Execute(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if result.Phase != "complete" || m.owner != "" {
		t.Fatal("incomplete operation")
	}
	for name, o := range b.nodes {
		if hash(o.Components["core"]) != hash(s.Nodes[name].Components["core"]) || hash(o.Components["helper"]) != hash(s.Nodes[name].Components["helper"]) {
			t.Fatal("unselected workload changed")
		}
	}
	count := len(b.calls)
	if _, e = r.Execute(ctx, p); e != nil {
		t.Fatal(e)
	}
	for _, q := range b.calls[count:] {
		if q.Action != "observe" {
			t.Fatal("completed replay mutated")
		}
	}
}
func TestManagedLostResponseResumesSameNode(t *testing.T) {
	s, r, m, b := setup(t)
	enroll(t, s, r)
	p, e := Build(s, "upgrade", "tenderdash", "", pins(s, "tenderdash", "b"), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	lost := false
	b.after = func(q Request) error {
		if q.Action == "apply" && !lost {
			lost = true
			return errors.New("lost-after-apply")
		}
		return nil
	}
	ctx, c := deadline()
	defer c()
	_, e = r.Execute(ctx, p)
	if e == nil || m.r.Phase != "interrupted" || m.r.Current != s.Fleet.Targets[0].Name {
		t.Fatal("lost pending target", e)
	}
	pending := m.r.Current
	id := b.nodes[pending].Components["tenderdash"].ID
	b.after = nil
	start := len(b.calls)
	result, e := r.Execute(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if result.Phase != "complete" || b.nodes[pending].Components["tenderdash"].ID != id {
		t.Fatal("resume replaced accepted image")
	}
	for _, q := range b.calls[start:] {
		if q.Action == "apply" {
			if q.Target.Name != pending {
				t.Fatal("did not reconcile pending first")
			}
			break
		}
	}
}
func TestManagedScopeOwnershipStagingAndQuorumGates(t *testing.T) {
	for _, fault := range []string{"cloud", "claimed", "stage", "member", "core"} {
		t.Run(fault, func(t *testing.T) {
			s, r, m, b := setup(t)
			enroll(t, s, r)
			p, e := Build(s, "upgrade", "platform", "", pins(s, "platform", "b"), time.Now())
			if e != nil {
				t.Fatal(e)
			}
			switch fault {
			case "cloud":
				r.Cloud.(*cloud).foreign = true
			case "claimed":
				m.owner = "other"
			case "stage":
				b.fail = "stage"
				b.failOnce = true
			case "member":
				for n, o := range b.nodes {
					o.Chain.VotingPower = map[string]int64{h64("unmanaged"): 100}
					b.nodes[n] = o
				}
			case "core":
				n := s.Fleet.Targets[0].Name
				o := b.nodes[n]
				v := o.Components["core"]
				v.StartedAt = "2026-09-24T01:00:00Z"
				o.Components["core"] = v
				b.nodes[n] = o
			}
			ctx, c := deadline()
			defer c()
			start := len(b.calls)
			if _, e = r.Execute(ctx, p); e == nil {
				t.Fatal("unsafe change accepted")
			}
			for _, q := range b.calls[start:] {
				if q.Action == "apply" {
					t.Fatal("mutated after gate failure")
				}
			}
		})
	}
}
func TestManagedDeployRestoresStoppedImagesAndRejectsStalePlan(t *testing.T) {
	s, r, m, b := setup(t)
	enroll(t, s, r)
	p, e := Build(s, "deploy", "platform", "", pins(s, "platform", "a"), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	for n, o := range b.nodes {
		for c, v := range o.Components {
			if Selected("platform", c) {
				v.Running = false
				o.Components[c] = v
			}
		}
		b.nodes[n] = o
	}
	ctx, c := deadline()
	defer c()
	result, e := r.Execute(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if result.Phase != "complete" {
		t.Fatal("restore incomplete")
	}
	old, e := Build(s, "upgrade", "core", "", pins(s, "core", "b"), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Execute(ctx, old); e == nil {
		t.Fatal("stale predecessor accepted")
	}
	if m.r.OperationID != p.ID {
		t.Fatal("old intent overwrote current runtime")
	}
}
func TestManagedSnapshotAndPlanValidation(t *testing.T) {
	s, _, _, _ := setup(t)
	p, e := Build(s, "upgrade", "platform", "", pins(s, "platform", "b"), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	p.Images[s.Fleet.Targets[0].Name]["core"] = "docker.io/dashpay/core@sha256:" + strings.Repeat("b", 64)
	p.ID = ""
	p.ID = hash(p)
	if p.Validate() == nil {
		t.Fatal("scope escape accepted")
	}
	s.Nodes[s.Fleet.Targets[0].Name] = Observation{InstanceID: s.Fleet.Targets[0].InstanceID, Error: "unreachable"}
	s.ID = ""
	s.ID = hash(s)
	if s.Complete() == nil {
		t.Fatal("unknown target accepted")
	}
}
