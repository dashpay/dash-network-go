package join

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
	"strings"
	"testing"
	"time"
)

type backend struct {
	calls []string
	lost  bool
	wrong bool
}

func (f *backend) Call(_ context.Context, q node.Request) (node.Observation, error) {
	f.calls = append(f.calls, q.Action)
	if q.Action == "join-start" && f.lost {
		f.lost = false
		return node.Observation{}, errors.New("accepted start, lost response")
	}
	c := &node.Core{Genesis: q.Join.Genesis, CheckpointHash: q.Join.CheckpointHash, Height: 100, Headers: 100, Synced: true, Peers: 2, ContainerID: strings.Repeat("d", 64), ChainLockHeight: 99}
	if f.wrong {
		c.CheckpointHash = strings.Repeat("e", 64)
	}
	return node.Observation{InstanceID: q.Target.InstanceID, PlanID: q.Context.PlanID, Action: q.Action, Core: c}, nil
}
func setup(t *testing.T) (Plan, Runner, *memoryStore, *backend) {
	t.Helper()
	n := testutil.ProvisionNetwork(t)
	n.Metadata.Name = "testnet-new-core"
	n.Chain.Type = "testnet"
	n.Nodes[0].Role = "fullnode"
	cloud := &testutil.Cloud{Network: n}
	s := &memoryStore{}
	identity := testutil.Identity{Account: n.AWS.AccountID}
	cp, e := provision.Prepare(context.Background(), n, identity, cloud)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = provision.Execute(context.Background(), cp, identity, cloud, s, "provision", "test", time.Millisecond, nil); e != nil {
		t.Fatal(e)
	}
	for i := range cloud.Instances {
		cloud.Instances[i].PrivateIpAddress = aws.String(fmt.Sprintf("10.0.0.%d", i+10))
	}
	b, e := bootstrap.Build(cp, testutil.Lock(t, n), bootstrap.Access{User: "ubuntu", Port: 22, Address: "private"})
	if e != nil {
		t.Fatal(e)
	}
	s.record.Bootstrap = &provision.BootstrapProgress{PlanID: b.ID, Phase: "hosts-ready", Nodes: map[string]provision.BootstrapNode{}}
	for _, t := range b.Targets {
		s.record.Bootstrap.Nodes[t.Name] = provision.BootstrapNode{Phase: "ready", ObservedAt: time.Now(), DockerVersion: "28.2.2", ComposeVersion: "2.37.1"}
	}
	live, e := provision.RunningTargets(context.Background(), cp, s.record, cloud)
	if e != nil {
		t.Fatal(e)
	}
	p, e := Build(b, node.CoreJoin{ChainType: "testnet", CoreNetwork: "test", Genesis: strings.Repeat("a", 64), CheckpointHeight: 80, CheckpointHash: strings.Repeat("b", 64), Peers: []string{"10.0.1.2:19999"}, Options: []string{}}, live)
	if e != nil {
		t.Fatal(e)
	}
	f := &backend{}
	return p, Runner{Identity: identity, Cloud: cloud, Store: s, Remote: f, Owner: "join", Wait: func(ctx context.Context) error { return errors.New("test interrupted while syncing") }}, s, f
}
func run(t *testing.T, p Plan, r Runner) (provision.Record, error) {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), time.Second*5)
	defer c()
	return r.Execute(ctx, p)
}
func TestJoinExistingChainAndResumeLostResponse(t *testing.T) {
	p, r, s, f := setup(t)
	f.lost = true
	_, e := run(t, p, r)
	if e == nil || s.owner != "" || s.record.Join.Phase != "interrupted" {
		t.Fatal("lost response not retained", e)
	}
	v, e := run(t, p, r)
	if e != nil || v.Join.Phase != "joined" {
		t.Fatal(e)
	}
	for _, a := range f.calls {
		if a != "join-start" && a != "join-status" {
			t.Fatal("unexpected mutating capability")
		}
	}
	for _, v := range s.record.Join.Nodes {
		if v.Phase != "ready" {
			t.Fatal("target lost")
		}
	}
}
func TestWrongCheckpointNotHealthy(t *testing.T) {
	p, r, s, f := setup(t)
	f.wrong = true
	if _, e := run(t, p, r); e == nil || s.record.Join.Phase != "interrupted" {
		t.Fatal("foreign chain passed")
	}
}
func TestJoinRejectsChangedHostAndBusyOwner(t *testing.T) {
	p, r, s, f := setup(t)
	s.owner = "another"
	if _, e := run(t, p, r); e == nil || len(f.calls) > 0 {
		t.Fatal("busy runner bypassed")
	}
	s.owner = ""
	cloud := r.Cloud.(*testutil.Cloud)
	cloud.Instances[0].PrivateIpAddress = aws.String("10.0.0.99")
	if _, e := run(t, p, r); e == nil || len(f.calls) > 0 {
		t.Fatal("host drift bypassed")
	}
}
func TestJoinPlanCannotChangeReviewedChain(t *testing.T) {
	p, _, _, _ := setup(t)
	p.Chain.CheckpointHash = strings.Repeat("f", 64)
	if p.Validate() == nil {
		t.Fatal("altered chain accepted")
	}
}
func TestJoinCannotBecomeGenesisLifecycle(t *testing.T) {
	p, r, s, _ := setup(t)
	if _, e := run(t, p, r); e != nil {
		t.Fatal(e)
	}
	s.record.Deployment = &provision.DeploymentProgress{PlanID: strings.Repeat("d", 64)}
	if s.record.Validate(p.Bootstrap.Compute) == nil {
		t.Fatal("two engines own allocation")
	}
}
