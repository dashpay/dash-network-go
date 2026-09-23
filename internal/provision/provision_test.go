package provision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

type memoryStore struct {
	mu       sync.Mutex
	record   Record
	owner    string
	failSave func(Record) error
}

func copyRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var c Record
	_ = json.Unmarshal(b, &c)
	return c
}
func (s *memoryStore) Acquire(_ context.Context, p Plan, o string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != "" {
		return Record{}, errors.New("busy")
	}
	if s.record.Kind == "" {
		s.record = NewRecord(p)
	}
	if s.record.Plan.ID != p.ID {
		return Record{}, errors.New("different plan")
	}
	s.owner = o
	return copyRecord(s.record), nil
}
func (s *memoryStore) Save(_ context.Context, r Record, o string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o != s.owner {
		return errors.New("lost owner")
	}
	if r.Revision != s.record.Revision+1 {
		return errors.New("stale revision")
	}
	if s.failSave != nil {
		if err := s.failSave(r); err != nil {
			return err
		}
	}
	s.record = copyRecord(r)
	return nil
}
func (s *memoryStore) Release(_ context.Context, _ Plan, o string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o != s.owner {
		return errors.New("wrong owner")
	}
	s.owner = ""
	return nil
}
func setup(t *testing.T) (Plan, *testutil.Cloud, *memoryStore) {
	t.Helper()
	n := testutil.ProvisionNetwork(t)
	c := &testutil.Cloud{Network: n}
	p, err := Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, c)
	if err != nil {
		t.Fatal(err)
	}
	return p, c, &memoryStore{}
}
func run(ctx context.Context, p Plan, c *testutil.Cloud, s *memoryStore, owner string) (Record, error) {
	return Execute(ctx, p, testutil.Identity{Account: p.Network.AWS.AccountID}, c, s, owner, "test-revision", time.Millisecond, nil)
}
func TestCreateAndResumeWithoutDuplicateLaunches(t *testing.T) {
	p, c, s := setup(t)
	r, err := run(context.Background(), p, c, s, "runner-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != "compute-ready" || r.ApplicationHealth != "unknown" || len(r.Nodes) != 2 || s.owner != "" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if len(c.Requests) != 2 {
		t.Fatal("incorrect launch count")
	}
	for _, req := range c.Requests {
		disk := req.BlockDeviceMappings[0].Ebs
		if !aws.ToBool(disk.Encrypted) || aws.ToBool(disk.DeleteOnTermination) || disk.VolumeType != types.VolumeTypeGp3 || aws.ToInt32(req.MinCount) != 1 || aws.ToInt32(req.MaxCount) != 1 || req.MetadataOptions.HttpTokens != types.HttpTokensStateRequired || aws.ToBool(req.NetworkInterfaces[0].AssociatePublicIpAddress) || len(req.TagSpecifications) != 2 || len(aws.ToString(req.ClientToken)) != 64 {
			t.Fatal("unsafe launch contract")
		}
	}
	_, err = run(context.Background(), p, c, s, "runner-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Requests) != 2 {
		t.Fatal("resume duplicated resources")
	}
}
func TestLostLaunchResponseReconcilesBeforeContinuing(t *testing.T) {
	p, c, s := setup(t)
	c.AfterRun = func() error { return errors.New("response lost") }
	r, err := run(context.Background(), p, c, s, "runner-1")
	if err == nil || r.Nodes[p.Targets[0].Name].Phase != "launching" || s.owner != "" {
		t.Fatal("uncertain launch not preserved")
	}
	c.AfterRun = nil
	r, err = run(context.Background(), p, c, s, "runner-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Requests) != 2 || r.Phase != "compute-ready" || r.LastError != "" {
		t.Fatal("lost response produced duplicate/missing resources")
	}
}
func TestUnknownLaunchWithoutVisibleInstanceNeverRetries(t *testing.T) {
	p, c, s := setup(t)
	c.BeforeRun = func(*ec2.RunInstancesInput) error { return errors.New("transport unavailable") }
	if _, err := run(context.Background(), p, c, s, "runner-1"); err == nil {
		t.Fatal("expected failure")
	}
	c.BeforeRun = nil
	_, err := run(context.Background(), p, c, s, "runner-2")
	if err == nil || !strings.Contains(err.Error(), "no automatic relaunch") || len(c.Requests) != 0 {
		t.Fatal("uncertain launch automatically retried", err)
	}
	if s.record.LastError == "" || s.record.Nodes[p.Targets[0].Name].EC2State != "unknown" {
		t.Fatal("uncertain target or diagnostic lost from shared journal")
	}
	if len(s.record.Nodes) != 2 || s.record.Nodes[p.Targets[1].Name].Phase != "pending" {
		t.Fatal("unattempted target disappeared")
	}
}
func TestFailedJournalAfterLaunchAndLostProcessRecovers(t *testing.T) {
	p, c, s := setup(t)
	s.failSave = func(r Record) error {
		if r.Nodes[p.Targets[0].Name].InstanceID != "" {
			return errors.New("journal unavailable")
		}
		return nil
	}
	if _, err := run(context.Background(), p, c, s, "runner-1"); err == nil {
		t.Fatal("expected failure")
	}
	if s.record.Nodes[p.Targets[0].Name].Phase != "launching" || len(c.Requests) != 1 {
		t.Fatal("pre-launch record missing")
	}
	s.failSave = nil
	if _, err := run(context.Background(), p, c, s, "runner-2"); err != nil {
		t.Fatal(err)
	}
	if len(c.Requests) != 2 {
		t.Fatal("duplicate after failed checkpoint")
	}
}
func TestCompetingRunnersDoNotLaunch(t *testing.T) {
	p, c, s := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	c.BeforeRun = func(*ec2.RunInstancesInput) error {
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := run(context.Background(), p, c, s, "first"); done <- err }()
	<-entered
	if _, err := run(context.Background(), p, c, s, "second"); err == nil {
		t.Fatal("second runner acquired claim")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(c.Requests) != 2 {
		t.Fatal("competing runner launched")
	}
}
func TestPreflightFailuresHaveNoJournalOrMutation(t *testing.T) {
	for _, name := range []string{"ami-owner", "ami-architecture", "ami-extra-disk", "subnet-vpc", "instance-architecture"} {
		t.Run(name, func(t *testing.T) {
			p, c, s := setup(t)
			switch name {
			case "ami-owner":
				c.ImageHook = func(i *types.Image) { i.OwnerId = aws.String("000000000000") }
			case "ami-architecture":
				c.ImageHook = func(i *types.Image) { i.Architecture = types.ArchitectureValuesX8664 }
			case "ami-extra-disk":
				c.ImageHook = func(i *types.Image) {
					i.BlockDeviceMappings = append(i.BlockDeviceMappings, types.BlockDeviceMapping{})
				}
			case "subnet-vpc":
				c.SubnetHook = func(s *types.Subnet) { s.VpcId = aws.String("vpc-00000002") }
			case "instance-architecture":
				c.TypeHook = func(i *types.InstanceTypeInfo) { i.ProcessorInfo.SupportedArchitectures = nil }
			}
			if _, err := run(context.Background(), p, c, s, "runner"); err == nil {
				t.Fatal("unsafe preflight passed")
			}
			if len(c.Requests) != 0 || s.record.Kind != "" {
				t.Fatal("preflight failure mutated state")
			}
		})
	}
	p, c, s := setup(t)
	if _, err := Execute(context.Background(), p, testutil.Identity{Account: "000000000000"}, c, s, "runner", "test", time.Millisecond, nil); err == nil || s.record.Kind != "" {
		t.Fatal("account mismatch accepted")
	}
}
func TestAllLiveTargetsValidatedBeforeMutation(t *testing.T) {
	for _, name := range []string{"foreign", "duplicate", "terminated", "tag-drift", "type-drift", "imds-drift", "pagination-error", "pagination-repeat"} {
		t.Run(name, func(t *testing.T) {
			p, c, s := setup(t)
			if _, err := run(context.Background(), p, c, s, "initial"); err != nil {
				t.Fatal(err)
			}
			c.Requests = nil
			switch name {
			case "foreign":
				c.Instances[0].Tags = nil
			case "duplicate":
				c.Instances = append(c.Instances, c.Instances[0])
			case "terminated":
				c.Instances[0].State.Name = types.InstanceStateNameTerminated
			case "tag-drift":
				c.Instances[0].Tags[0].Value = aws.String("other")
			case "type-drift":
				c.Instances[0].InstanceType = types.InstanceTypeT3Micro
			case "imds-drift":
				c.Instances[0].MetadataOptions.HttpTokens = types.HttpTokensStateOptional
			case "pagination-error", "pagination-repeat":
				c.Describe = func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
					if in.NextToken != nil && name == "pagination-error" {
						return nil, errors.New("page 2 failed")
					}
					return &ec2.DescribeInstancesOutput{NextToken: aws.String("repeat")}, nil
				}
			}
			if _, err := run(context.Background(), p, c, s, "resume"); err == nil {
				t.Fatal("unsafe live state accepted")
			}
			if len(c.Requests) != 0 {
				t.Fatal("drift caused new launches")
			}
		})
	}
}
func TestCancelledWaitKeepsTargetsAndReleasesClaim(t *testing.T) {
	p, c, s := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.AfterRun = func() error {
		if len(c.Instances) == 2 {
			for i := range c.Instances {
				c.Instances[i].State.Name = types.InstanceStateNamePending
			}
			cancel()
		}
		return nil
	}
	r, err := run(ctx, p, c, s, "cancelled")
	if !errors.Is(err, context.Canceled) || r.Phase != "interrupted" || len(r.Nodes) != 2 || s.owner != "" {
		t.Fatal("cancellation lost operation or lock", err)
	}
}
func TestPlanChangesAndGenerationsCannotBypassExistingNetwork(t *testing.T) {
	p, c, s := setup(t)
	if _, err := run(context.Background(), p, c, s, "initial"); err != nil {
		t.Fatal(err)
	}
	c.Network.Chain.Generation++
	next, err := Prepare(context.Background(), c.Network, testutil.Identity{Account: p.Network.AWS.AccountID}, c)
	if err != nil {
		t.Fatal(err)
	}
	if next.Key() != p.Key() || next.ID == p.ID {
		t.Fatal("generation lock scope invalid")
	}
	if _, err = run(context.Background(), next, c, s, "next"); err == nil {
		t.Fatal("silently rotated network")
	}
	p.Targets = p.Targets[:1]
	if err = p.Validate(); err == nil {
		t.Fatal("partial plan accepted")
	}
}

func TestOwnershipLostBeforeLaunchStopsWithoutReplacingClaim(t *testing.T) {
	p, c, s := setup(t)
	s.failSave = func(r Record) error {
		if r.Nodes[p.Targets[0].Name].Phase == "launching" {
			// Model an explicitly recovered claim: stale runner must stop at its next
			// conditional write, never issue another EC2 mutation or release new owner.
			s.owner = "replacement-runner"
			return errors.New("lost owner")
		}
		return nil
	}
	if _, err := run(context.Background(), p, c, s, "old-runner"); err == nil {
		t.Fatal("stale runner continued")
	}
	if len(c.Requests) != 0 || s.owner != "replacement-runner" {
		t.Fatal("stale runner mutated EC2 or released replacement")
	}
}

func TestPostLaunchVisibilityLagWaitsWithoutRelaunch(t *testing.T) {
	p, c, s := setup(t)
	c.Describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		if c.Reads <= 2 {
			return &ec2.DescribeInstancesOutput{}, nil
		}
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{OwnerId: aws.String(p.Network.AWS.AccountID), Instances: c.Instances}}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := run(ctx, p, c, s, "runner")
	if err != nil || r.Phase != "compute-ready" || c.Reads < 3 || len(c.Requests) != 2 {
		t.Fatal("visibility lag mishandled", err)
	}
}
