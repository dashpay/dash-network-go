package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

func digest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }

type fakeRemote struct {
	mu            sync.Mutex
	calls         []node.Request
	counts        map[string]int
	registrations map[string]string
	before        func(node.Request) error
	after         func(node.Request, *node.Observation) error
}

func (f *fakeRemote) Call(ctx context.Context, q node.Request) (node.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := node.Observation{InstanceID: q.Target.InstanceID, PlanID: q.Context.PlanID, Action: q.Action}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	f.calls = append(f.calls, q)
	if f.before != nil {
		if err := f.before(q); err != nil {
			return o, err
		}
	}
	id := digest(q.Target.Name)
	f.counts[q.Target.Name]++
	height := int64(500 + f.counts[q.Target.Name])
	switch q.Action {
	case "core-start", "core-finalize", "core-status":
		o.Core = &node.Core{Genesis: digest("genesis"), Height: height, Headers: height, Peers: 13, ContainerID: digest("core" + q.Target.Name), ConfigSHA256: digest("config"), MasternodeState: "READY", ProTxHash: digest("protx" + q.Target.Name), ChainLockHeight: height - 1, Quorums: map[string]int{"llmq_devnet": 4, "llmq_devnet_dip0024": 2, "llmq_devnet_platform": 4}}
	case "wallet":
		o.PayoutAddress = "y" + strings.Repeat("1", 33)
		o.SporkAddress = "y" + strings.Repeat("2", 33)
	case "identity":
		o.OperatorPublicKey = id + id[:32]
		o.PlatformNodeID = id[:40]
	case "register":
		o.ProTxHash = digest("protx" + q.Registration.Name)
		o.Confirmations = 1
		f.registrations[q.Registration.Name] = o.ProTxHash
	case "platform-status":
		o.Platform = &node.Platform{Height: height, DAPIHeight: height, ChainID: q.Context.PlatformChainID, NodeID: id[:40], ProTxHash: digest("protx" + q.Target.Name), DriveVersion: "4.2.0", ReferenceBlockHash: digest("block"), Containers: map[string]string{"drive": digest("drive"), "tenderdash": digest("td"), "dapi": digest("dapi"), "gateway": digest("gateway")}}
	}
	if f.after != nil {
		if err := f.after(q, &o); err != nil {
			return node.Observation{}, err
		}
	}
	return o, nil
}
func setup(t *testing.T) (Plan, Runner, *memoryStore, *fakeRemote) {
	t.Helper()
	n := testutil.ProvisionNetwork(t)
	n.Nodes[0].Count = 13
	n.Nodes = append(n.Nodes, spec.NodeGroup{Name: "wallet", Role: "wallet", Count: 1, Architecture: n.Nodes[0].Architecture, InstanceType: n.Nodes[0].InstanceType})
	c := &testutil.Cloud{Network: n}
	s := &memoryStore{}
	p, err := provision.Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, c)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provision.Execute(context.Background(), p, testutil.Identity{Account: n.AWS.AccountID}, c, s, "compute", "test", time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range c.Instances {
		c.Instances[i].PrivateIpAddress = aws.String(fmt.Sprintf("10.0.0.%d", i+10))
	}
	b, err := bootstrap.Build(p, testutil.Lock(t, n), bootstrap.Access{User: "ubuntu", Port: 22, Address: "private"})
	if err != nil {
		t.Fatal(err)
	}
	s.record.Bootstrap = &provision.BootstrapProgress{PlanID: b.ID, Phase: "hosts-ready", Nodes: map[string]provision.BootstrapNode{}}
	for _, x := range b.Targets {
		s.record.Bootstrap.Nodes[x.Name] = provision.BootstrapNode{Phase: "ready", ObservedAt: time.Now(), DockerVersion: "28.2.2", ComposeVersion: "2.37.1"}
	}
	live, err := provision.RunningTargets(context.Background(), p, s.record, c)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(b, live, 14, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRemote{counts: map[string]int{}, registrations: map[string]string{}}
	r := Runner{Identity: testutil.Identity{Account: n.AWS.AccountID}, Cloud: c, Store: s, Remote: f, Owner: "deploy", Version: "test", Wait: func(ctx context.Context) error { return ctx.Err() }}
	return plan, r, s, f
}
func execute(t *testing.T, p Plan, r Runner) (provision.Record, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return r.Execute(ctx, p, false)
}
func TestCompleteLifecycleResumeDoctorAndStop(t *testing.T) {
	p, r, s, f := setup(t)
	a, err := execute(t, p, r)
	if err != nil {
		t.Fatal(err)
	}
	if a.Deployment.Phase != "network-ready" || len(a.Deployment.Nodes) != 14 || s.owner != "" {
		t.Fatal("incomplete readiness")
	}
	height := a.Deployment.GenesisCoreHeight
	b, err := execute(t, p, r)
	if err != nil {
		t.Fatal(err)
	}
	if b.Deployment.GenesisCoreHeight != height || len(f.registrations) != 13 {
		t.Fatal("resume changed genesis or registrations")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := len(f.calls)
	h, err := r.Doctor(ctx, p, b)
	if err != nil || !h.Healthy {
		t.Fatal(h, err)
	}
	for _, q := range f.calls[start:] {
		if q.Action != "core-status" && q.Action != "platform-status" {
			t.Fatal("doctor mutated", q.Action)
		}
	}
	stopped, err := r.Execute(ctx, p, true)
	if err != nil || stopped.Deployment.Phase != "stopped" {
		t.Fatal(err)
	}
	if stopped.Deployment.Nodes[p.Validators()[0].Name].ProTxHash == "" {
		t.Fatal("stop lost identity")
	}
}
func TestAllTargetsPreflightBeforeMutation(t *testing.T) {
	p, r, s, f := setup(t)
	f.before = func(q node.Request) error {
		if q.Target.Name == p.Targets[3].Name {
			return errors.New("host key mismatch")
		}
		return nil
	}
	_, err := execute(t, p, r)
	if err == nil {
		t.Fatal("accepted unreachable host")
	}
	if len(f.calls) != 14 {
		t.Fatal("silently dropped targets", len(f.calls))
	}
	for _, q := range f.calls {
		if q.Action != "inspect" {
			t.Fatal("mutated before fleet preflight")
		}
	}
	if s.record.Deployment.Phase != "interrupted" || s.record.LastError == "" {
		t.Fatal("lost interruption")
	}
}
func TestLostRegistrationAndPlatformResponseResume(t *testing.T) {
	for _, action := range []string{"register", "platform-start"} {
		t.Run(action, func(t *testing.T) {
			p, r, s, f := setup(t)
			once := false
			f.after = func(q node.Request, o *node.Observation) error {
				if q.Action == action && !once {
					once = true
					return errors.New("response lost")
				}
				return nil
			}
			_, err := execute(t, p, r)
			if err == nil {
				t.Fatal("lost response accepted")
			}
			initial := s.record.Deployment.GenesisCoreHeight
			f.after = nil
			got, err := execute(t, p, r)
			if err != nil {
				t.Fatal(err)
			}
			if initial != 0 && initial != got.Deployment.GenesisCoreHeight {
				t.Fatal("genesis rewritten")
			}
			if len(f.registrations) != 13 {
				t.Fatal("registrations missing")
			}
		})
	}
}
func TestChangedHostAndPlanRefused(t *testing.T) {
	p, r, _, f := setup(t)
	p.Targets[0].PeerAddress = "10.5.5.5"
	p.ID = ""
	p.ID = hash(p)
	_, err := execute(t, p, r)
	if err == nil || len(f.calls) > 0 {
		t.Fatal("address drift accepted")
	}
	p.RecipeSHA256 = strings.Repeat("0", 64)
	p.ID = ""
	p.ID = hash(p)
	if p.Validate() == nil {
		t.Fatal("unknown recipe accepted")
	}
}
func TestDoctorNeverHidesUnreachableOrDivergentNode(t *testing.T) {
	for _, kind := range []string{"unreachable", "stalled", "fork", "wrong-identity"} {
		t.Run(kind, func(t *testing.T) {
			p, r, s, f := setup(t)
			if _, err := execute(t, p, r); err != nil {
				t.Fatal(err)
			}
			name := p.Validators()[3].Name
			f.after = func(q node.Request, o *node.Observation) error {
				if q.Target.Name == name && q.Action == "platform-status" {
					switch kind {
					case "unreachable":
						return errors.New("SSH lost")
					case "stalled":
						o.Platform.Height = 1
					case "fork":
						o.Platform.ReferenceBlockHash = digest("fork")
					case "wrong-identity":
						o.Platform.NodeID = strings.Repeat("0", 40)
					}
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			h, err := r.Doctor(ctx, p, s.record)
			if err != nil || h.Healthy || len(h.Nodes) != 14 || h.Nodes[name].Healthy {
				t.Fatal("bad health projection", h, err)
			}
		})
	}
}
func TestBusyClaimAndDeadlineStopBeforeRemote(t *testing.T) {
	p, r, s, f := setup(t)
	s.owner = "other"
	if _, err := execute(t, p, r); err == nil {
		t.Fatal("stole claim")
	}
	if len(f.calls) != 0 {
		t.Fatal("remote work before claim")
	}
	s.owner = ""
	if _, err := r.Execute(context.Background(), p, false); err == nil {
		t.Fatal("no deadline accepted")
	}
}
