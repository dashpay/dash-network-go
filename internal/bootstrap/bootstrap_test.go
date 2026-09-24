package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
	"github.com/dashpay/dash-network-go/internal/transport"
)

type memoryStore struct {
	record   provision.Record
	owner    string
	failSave func(provision.Record) error
}

func clone(r provision.Record) provision.Record {
	b, _ := json.Marshal(r)
	var c provision.Record
	_ = json.Unmarshal(b, &c)
	return c
}
func (s *memoryStore) Read(_ context.Context, p provision.Plan) (provision.Record, string, error) {
	if s.record.Kind == "" {
		return provision.Record{}, "", errors.New("missing")
	}
	return clone(s.record), s.owner, s.record.Validate(p)
}
func (s *memoryStore) Acquire(_ context.Context, p provision.Plan, o string) (provision.Record, error) {
	if s.owner != "" {
		return provision.Record{}, errors.New("busy")
	}
	if s.record.Kind == "" {
		s.record = provision.NewRecord(p)
	}
	if s.record.Plan.ID != p.ID {
		return provision.Record{}, errors.New("plan changed")
	}
	s.owner = o
	return clone(s.record), nil
}
func (s *memoryStore) Save(_ context.Context, r provision.Record, o string) error {
	if o != s.owner || r.Revision != s.record.Revision+1 {
		return errors.New("owner/revision lost")
	}
	if err := r.Validate(r.Plan); err != nil {
		return err
	}
	if s.failSave != nil {
		if err := s.failSave(r); err != nil {
			return err
		}
	}
	s.record = clone(r)
	return nil
}
func (s *memoryStore) Release(_ context.Context, _ provision.Plan, o string) error {
	if o != s.owner {
		return errors.New("wrong owner")
	}
	s.owner = ""
	return nil
}

type remoteFake struct {
	plan            Plan
	prepared        map[string]bool
	applies, probes int
	before          func(string, string) error
	afterApply      func(string) error
	response        func(*Observation)
}

func (f *remoteFake) Run(ctx context.Context, e transport.Endpoint, command, script string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := strings.Split(e.HostAlias, ".")[0]
	mode := "probe"
	if strings.Contains(command, " 'apply' ") {
		mode = "apply"
	}
	if f.before != nil {
		if err := f.before(mode, id); err != nil {
			return nil, err
		}
	}
	if mode == "apply" {
		f.applies++
		f.prepared[id] = true
		if f.afterApply != nil {
			if err := f.afterApply(id); err != nil {
				return nil, err
			}
		}
	} else {
		f.probes++
	}
	o := Observation{PlanID: f.plan.ID, InstanceID: id, Architecture: f.plan.Targets[0].Architecture, Ready: f.prepared[id], DockerVersion: "28.2.2", ComposeVersion: "2.37.1"}
	if f.response != nil {
		f.response(&o)
	}
	b, _ := json.Marshal(o)
	return b, nil
}
func setup(t *testing.T) (Plan, *testutil.Cloud, *memoryStore, *remoteFake) {
	t.Helper()
	n := testutil.ProvisionNetwork(t)
	c := &testutil.Cloud{Network: n}
	s := &memoryStore{}
	p, err := provision.Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provision.Execute(context.Background(), p, testutil.Identity{Account: n.AWS.AccountID}, c, s, "compute", "test", time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	for i := range c.Instances {
		c.Instances[i].PrivateIpAddress = aws.String(fmt.Sprintf("10.0.0.%d", i+10))
	}
	b, err := Build(p, testutil.Lock(t, n), Access{User: "ubuntu", Port: 22, Address: "private"})
	if err != nil {
		t.Fatal(err)
	}
	return b, c, s, &remoteFake{plan: b, prepared: map[string]bool{}}
}
func run(t *testing.T, p Plan, c *testutil.Cloud, s *memoryStore, f Remote) (provision.Record, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return Execute(ctx, p, testutil.Identity{Account: p.Compute.Network.AWS.AccountID}, c, s, f, "bootstrap", "test", nil)
}
func TestPrepareResumeAndReadback(t *testing.T) {
	p, c, s, f := setup(t)
	for i := 0; i < 2; i++ {
		r, err := run(t, p, c, s, f)
		if err != nil {
			t.Fatal(err)
		}
		if r.Bootstrap.Phase != "hosts-ready" || r.Phase != "compute-ready" || r.ApplicationHealth != "unknown" || s.owner != "" {
			t.Fatal("incorrect readiness boundary")
		}
	}
	if f.applies != 2 || f.probes != 10 || len(c.Requests) != 2 {
		t.Fatalf("resume repeated mutation or skipped readback: %d/%d", f.applies, f.probes)
	}
}
func TestLostSSHResponseReconcilesBeforeApplying(t *testing.T) {
	p, c, s, f := setup(t)
	f.afterApply = func(string) error { return errors.New("response lost") }
	r, err := run(t, p, c, s, f)
	if err == nil || r.Bootstrap.Phase != "interrupted" || r.Bootstrap.Nodes[p.Targets[0].Name].Phase != "unknown" || s.owner != "" || !strings.Contains(r.LastError, "response lost") {
		t.Fatal("lost response not retained", err)
	}
	f.afterApply = nil
	r, err = run(t, p, c, s, f)
	if err != nil || r.Bootstrap.Phase != "hosts-ready" || f.applies != 2 || r.LastError != "" {
		t.Fatal("unsafe or incomplete recovery", err)
	}
}
func TestEveryHostPreflightBeforeAnyMutation(t *testing.T) {
	p, c, s, f := setup(t)
	f.before = func(mode, id string) error {
		if id == "i-00000002" {
			return errors.New("host key changed")
		}
		return nil
	}
	r, err := run(t, p, c, s, f)
	if err == nil || f.applies != 0 || len(r.Bootstrap.Nodes) != 2 || r.Bootstrap.Nodes[p.Targets[1].Name].Phase != "unknown" {
		t.Fatal("partial preflight led to mutation", err)
	}
}
func TestCloudDriftRefusedBeforeSSH(t *testing.T) {
	for _, which := range []string{"tags", "missing", "instance", "ip", "stopped"} {
		t.Run(which, func(t *testing.T) {
			p, c, s, f := setup(t)
			switch which {
			case "tags":
				c.Instances[0].Tags = nil
			case "missing":
				c.Instances = c.Instances[:1]
			case "instance":
				c.Instances[0].InstanceId = aws.String("i-ffffffff")
			case "ip":
				c.Instances[0].PrivateIpAddress = nil
			case "stopped":
				c.Instances[0].State.Name = "stopped"
			}
			if _, err := run(t, p, c, s, f); err == nil || f.applies != 0 || f.probes != 0 {
				t.Fatal("unsafe SSH scope", err)
			}
		})
	}
}
func TestBootstrapUsesSameNetworkClaim(t *testing.T) {
	p, c, s, f := setup(t)
	s.owner = "other-runner"
	if _, err := run(t, p, c, s, f); err == nil || s.owner != "other-runner" || f.probes != 0 {
		t.Fatal("competing claim bypassed", err)
	}
}
func TestWrongAccountAndIncompleteComputeNeverClaim(t *testing.T) {
	p, c, s, f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Execute(ctx, p, testutil.Identity{Account: "999999999999"}, c, s, f, "bad", "test", nil); err == nil || s.record.Bootstrap != nil {
		t.Fatal("cross account accepted")
	}
	s.record.Phase = "interrupted"
	if _, err := run(t, p, c, s, f); err == nil || s.record.Bootstrap != nil || s.owner != "" {
		t.Fatal("incomplete compute accepted")
	}
}
func TestNewPlanCannotReplacePreparedHosts(t *testing.T) {
	p, c, s, f := setup(t)
	if _, err := run(t, p, c, s, f); err != nil {
		t.Fatal(err)
	}
	access := p.Access
	access.User = "root"
	next, err := Build(p.Compute, p.Release, access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run(t, next, c, s, f); err == nil || s.record.Bootstrap.PlanID != p.ID || f.applies != 2 {
		t.Fatal("bootstrap plan silently replaced")
	}
}
func TestStaleRunnerCannotIssueMutation(t *testing.T) {
	p, c, s, f := setup(t)
	s.failSave = func(r provision.Record) error {
		if r.Bootstrap != nil && r.Bootstrap.Nodes[p.Targets[0].Name].Phase == "preparing" {
			s.owner = "replacement"
			return errors.New("lost ownership")
		}
		return nil
	}
	if _, err := run(t, p, c, s, f); err == nil || f.applies != 0 || s.owner != "replacement" {
		t.Fatal("stale runner mutated/released")
	}
}
func TestHostBusyAndCancellationRemainRecoverable(t *testing.T) {
	p, c, s, f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f.before = func(mode, id string) error {
		if mode == "apply" {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	r, err := Execute(ctx, p, testutil.Identity{Account: p.Compute.Network.AWS.AccountID}, c, s, f, "cancelled", "test", nil)
	if !errors.Is(err, context.Canceled) || len(r.Bootstrap.Nodes) != 2 || s.owner != "" || r.Bootstrap.Phase != "interrupted" {
		t.Fatal("cancellation lost state", err)
	}
	f.before = func(mode, id string) error { return errors.New("host-lock: still running") }
	if _, err = run(t, p, c, s, f); err == nil || f.applies != 0 {
		t.Fatal("busy remote re-executed")
	}
}
func TestFinalFleetReadbackRejectsLostReadiness(t *testing.T) {
	p, c, s, f := setup(t)
	f.before = func(mode, id string) error {
		if mode == "probe" && f.applies == 2 && id == "i-00000001" {
			f.prepared[id] = false
		}
		return nil
	}
	r, err := run(t, p, c, s, f)
	if err == nil || r.Bootstrap.Phase == "hosts-ready" || r.Bootstrap.Nodes[p.Targets[0].Name].Phase != "unknown" {
		t.Fatal("stale success accepted")
	}
}
func TestRecipeAndPlanValidation(t *testing.T) {
	p, _, _, _ := setup(t)
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(recipe)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recipe syntax: %s %v", out, err)
	}
	for _, alter := range []func(*Plan){func(p *Plan) { p.ID = "bad" }, func(p *Plan) { p.RecipeSHA256 = "bad" }, func(p *Plan) { p.Targets = p.Targets[:1] }, func(p *Plan) { p.Access.User = "ubuntu;evil" }, func(p *Plan) { p.Release.SpecHash = "other" }} {
		b, _ := json.Marshal(p)
		var bad Plan
		_ = json.Unmarshal(b, &bad)
		alter(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("altered plan accepted")
		}
	}
	command, script, err := Command(p, p.Targets[0], "i-00000001", "apply")
	if err != nil || !strings.HasPrefix(command, "sudo -n -- /usr/bin/env -i") || digest([]byte(script)) != p.RecipeSHA256 {
		t.Fatal("recipe boundary", err)
	}
	if _, _, err = Command(p, p.Targets[0], "i-00000001;evil", "apply"); err == nil {
		t.Fatal("unvalidated instance passed to shell")
	}
}
func TestHostResponseIdentityAndPrivacy(t *testing.T) {
	for _, change := range []func(*Observation){func(o *Observation) { o.PlanID = "other" }, func(o *Observation) { o.InstanceID = "other" }, func(o *Observation) { o.Architecture = "other" }, func(o *Observation) { o.Ready = true; o.DockerVersion = "secret\nmaterial" }} {
		p, c, s, f := setup(t)
		f.response = change
		if _, err := run(t, p, c, s, f); err == nil || strings.Contains(s.record.LastError, "secret") || f.applies != 0 {
			t.Fatal("untrusted response accepted/leaked", err)
		}
	}
}

func TestCompletedHostWithLostCheckpointIsNotReapplied(t *testing.T) {
	p, c, s, f := setup(t)
	failed := false
	s.failSave = func(r provision.Record) error {
		if !failed && r.Bootstrap != nil && r.Bootstrap.Nodes[p.Targets[0].Name].Phase == "ready" {
			failed = true
			return errors.New("journal unavailable")
		}
		return nil
	}
	if _, err := run(t, p, c, s, f); err == nil || f.applies != 1 {
		t.Fatal("expected post-apply journal failure", err)
	}
	s.failSave = nil
	if _, err := run(t, p, c, s, f); err != nil || f.applies != 2 {
		t.Fatal("checkpoint loss caused a duplicate apply", err)
	}
}
