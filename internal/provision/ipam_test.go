package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

type addressCloud struct {
	*testutil.Cloud
	addresses                                     []types.Address
	allocations, associations                     int
	allocateBefore, allocateAfter, associateAfter func() error
	poolHook                                      func(*types.IpamPool)
	releases                                      int
	releaseAfter                                  func() error
}

func (c *addressCloud) DescribeIpamPools(context.Context, *ec2.DescribeIpamPoolsInput, ...func(*ec2.Options)) (*ec2.DescribeIpamPoolsOutput, error) {
	p := types.IpamPool{IpamPoolId: aws.String(c.Network.AWS.Provision.IPAMPoolID), OwnerId: aws.String(c.Network.AWS.AccountID), Locale: aws.String(c.Network.AWS.Region), State: "create-complete", AddressFamily: "ipv4", IpamScopeType: "public", PublicIpSource: "byoip", AwsService: "ec2"}
	if c.poolHook != nil {
		c.poolHook(&p)
	}
	return &ec2.DescribeIpamPoolsOutput{IpamPools: []types.IpamPool{p}}, nil
}
func (c *addressCloud) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	o, err := c.Cloud.DescribeInstances(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	for r := range o.Reservations {
		for j := range o.Reservations[r].Instances {
			i := &o.Reservations[r].Instances[j]
			i.NetworkInterfaces = []types.InstanceNetworkInterface{{NetworkInterfaceId: aws.String("eni-" + *i.InstanceId), Attachment: &types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)}}}
			for _, a := range c.addresses {
				if aws.ToString(a.InstanceId) == *i.InstanceId {
					i.PublicIpAddress = a.PublicIp
				}
			}
		}
	}
	return o, nil
}
func (c *addressCloud) DescribeAddresses(_ context.Context, in *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	if len(in.Filters) != 1 || aws.ToString(in.Filters[0].Name) != "tag:"+c.Network.AWS.NetworkTagKey || len(in.Filters[0].Values) != 1 || in.Filters[0].Values[0] != c.Network.Metadata.Name {
		return nil, errors.New("unscoped address read")
	}
	return &ec2.DescribeAddressesOutput{Addresses: c.addresses}, nil
}
func (c *addressCloud) AllocateAddress(_ context.Context, in *ec2.AllocateAddressInput, opts ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error) {
	o := ec2.Options{}
	for _, f := range opts {
		f(&o)
	}
	if o.RetryMaxAttempts != 1 || in.PublicIpv4Pool != nil || aws.ToString(in.IpamPoolId) != c.Network.AWS.Provision.IPAMPoolID || string(in.Domain) != "vpc" || aws.ToString(in.NetworkBorderGroup) != c.Network.AWS.Region || len(in.TagSpecifications) != 1 || in.TagSpecifications[0].ResourceType != types.ResourceTypeElasticIp {
		return nil, errors.New("unsafe allocation contract")
	}
	c.allocations++
	if c.allocateBefore != nil {
		if err := c.allocateBefore(); err != nil {
			return nil, err
		}
	}
	id, ip := fmt.Sprintf("eipalloc-%017x", c.allocations), fmt.Sprintf("198.51.100.%d", c.allocations)
	c.addresses = append(c.addresses, types.Address{AllocationId: aws.String(id), PublicIp: aws.String(ip), Domain: "vpc", NetworkBorderGroup: in.NetworkBorderGroup, PublicIpv4Pool: in.IpamPoolId, Tags: in.TagSpecifications[0].Tags})
	if c.allocateAfter != nil {
		if err := c.allocateAfter(); err != nil {
			return nil, err
		}
	}
	return &ec2.AllocateAddressOutput{AllocationId: aws.String(id), PublicIp: aws.String(ip)}, nil
}
func (c *addressCloud) AssociateAddress(_ context.Context, in *ec2.AssociateAddressInput, opts ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error) {
	o := ec2.Options{}
	for _, f := range opts {
		f(&o)
	}
	if o.RetryMaxAttempts != 1 || aws.ToBool(in.AllowReassociation) {
		return nil, errors.New("unsafe association")
	}
	c.associations++
	for i := range c.addresses {
		a := &c.addresses[i]
		if aws.ToString(a.AllocationId) != aws.ToString(in.AllocationId) {
			continue
		}
		a.InstanceId, a.NetworkInterfaceId, a.AssociationId = in.InstanceId, aws.String("eni-"+*in.InstanceId), aws.String(fmt.Sprintf("eipassoc-%017x", c.associations))
		if c.associateAfter != nil {
			if err := c.associateAfter(); err != nil {
				return nil, err
			}
		}
		return &ec2.AssociateAddressOutput{AssociationId: a.AssociationId}, nil
	}
	return nil, errors.New("missing allocation")
}

func ipamSetup(t *testing.T) (Plan, *addressCloud, *memoryStore) {
	n := testutil.ProvisionNetwork(t)
	n.AWS.Provision.PublicIPv4 = true
	n.AWS.Provision.IPAMPoolID = "ipam-pool-00000000000000001"
	c := &addressCloud{Cloud: &testutil.Cloud{Network: n}}
	p, err := Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, c)
	if err != nil {
		t.Fatal(err)
	}
	return p, c, &memoryStore{}
}
func ipamRun(p Plan, c *addressCloud, s *memoryStore) (Record, error) {
	return Execute(context.Background(), p, testutil.Identity{Account: p.Network.AWS.AccountID}, c, s, "owner", "test", time.Millisecond, nil)
}

func TestIPAMProvisionAndReplay(t *testing.T) {
	p, c, s := ipamSetup(t)
	r, err := ipamRun(p, c, s)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != "compute-ready" || c.allocations != 2 || c.associations != 2 {
		t.Fatal("incomplete public footprint")
	}
	for _, req := range c.Requests {
		if aws.ToBool(req.NetworkInterfaces[0].AssociatePublicIpAddress) {
			t.Fatal("Amazon public address requested")
		}
	}
	for _, n := range r.Nodes {
		if n.Address.Phase != "associated" || n.Address.AllocationID == "" {
			t.Fatal("address lost from journal")
		}
	}
	if _, err = ipamRun(p, c, s); err != nil {
		t.Fatal(err)
	}
	if c.allocations != 2 || c.associations != 2 {
		t.Fatal("resume created/reassociated addresses")
	}
}

func TestIPAMLostRepliesReconcileWithoutDuplicate(t *testing.T) {
	for _, stage := range []string{"allocate", "associate", "checkpoint"} {
		t.Run(stage, func(t *testing.T) {
			p, c, s := ipamSetup(t)
			lost := func() error { return errors.New("reply lost") }
			switch stage {
			case "allocate":
				c.allocateAfter = lost
			case "associate":
				c.associateAfter = lost
			case "checkpoint":
				s.failSave = func(r Record) error {
					a := r.Nodes[p.Targets[0].Name].Address
					if a != nil && a.AllocationID != "" {
						return lost()
					}
					return nil
				}
			}
			if _, err := ipamRun(p, c, s); err == nil {
				t.Fatal("missing injected failure")
			}
			c.allocateAfter, c.associateAfter, s.failSave = nil, nil, nil
			if _, err := ipamRun(p, c, s); err != nil {
				t.Fatal(err)
			}
			if c.allocations != 2 || c.associations != 2 {
				t.Fatal("duplicated a resource after lost response")
			}
		})
	}
}

func TestIPAMUnknownAllocationDoesNotRetry(t *testing.T) {
	p, c, s := ipamSetup(t)
	c.allocateBefore = func() error { return errors.New("transport failure") }
	if _, err := ipamRun(p, c, s); err == nil {
		t.Fatal("missing error")
	}
	c.allocateBefore = nil
	_, err := ipamRun(p, c, s)
	if err == nil || !strings.Contains(err.Error(), "no automatic reallocation") || c.allocations != 1 {
		t.Fatal("uncertain allocation repeated", err)
	}
}

func TestIPAMRejectsWrongPoolAndDrift(t *testing.T) {
	for _, change := range []string{"owner", "region", "source", "state", "family", "scope"} {
		t.Run(change, func(t *testing.T) {
			p, c, s := ipamSetup(t)
			c.poolHook = func(x *types.IpamPool) {
				switch change {
				case "owner":
					x.OwnerId = aws.String("000000000000")
				case "region":
					x.Locale = aws.String("us-east-1")
				case "source":
					x.PublicIpSource = "amazon"
				case "state":
					x.State = "delete-in-progress"
				case "family":
					x.AddressFamily = "ipv6"
				case "scope":
					x.IpamScopeType = "private"
				}
			}
			if _, err := ipamRun(p, c, s); err == nil || len(c.Requests) > 0 || c.allocations > 0 {
				t.Fatal("unsafe preflight")
			}
		})
	}
	for _, change := range []string{"duplicate", "foreign", "detached", "pool", "missing", "unjournaled"} {
		t.Run(change, func(t *testing.T) {
			p, c, s := ipamSetup(t)
			if _, err := ipamRun(p, c, s); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "duplicate":
				c.addresses = append(c.addresses, c.addresses[0])
			case "foreign":
				c.addresses[0].InstanceId = aws.String("i-foreign")
			case "detached":
				c.addresses[0].InstanceId = nil
				c.addresses[0].NetworkInterfaceId = nil
				c.addresses[0].AssociationId = nil
			case "pool":
				c.addresses[0].PublicIpv4Pool = aws.String("ipam-pool-00000000000000002")
			case "missing":
				c.addresses = c.addresses[1:]
			case "unjournaled":
				n := s.record.Nodes[p.Targets[0].Name]
				n.Address = nil
				s.record.Nodes[p.Targets[0].Name] = n
			}
			if _, err := ipamRun(p, c, s); err == nil {
				t.Fatal("drift accepted")
			}
			if c.allocations != 2 || c.associations != 2 {
				t.Fatal("drift mutated addresses")
			}
		})
	}
}

func TestNewPublicProvisionRequiresPoolButHistoricalPlansRemainReadable(t *testing.T) {
	n := testutil.ProvisionNetwork(t)
	n.AWS.Provision.PublicIPv4 = true
	p := build(n, map[string]string{"arm64": "/dev/sda1"})
	if err := p.Validate(); err != nil {
		t.Fatal("historical plan no longer readable", err)
	}
	if _, err := Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, &testutil.Cloud{Network: n}); err == nil {
		t.Fatal("new Amazon public addresses allowed")
	}
}

func (c *addressCloud) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, opts ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	o := ec2.Options{}
	for _, f := range opts {
		f(&o)
	}
	if o.RetryMaxAttempts != 1 || aws.ToString(in.NetworkBorderGroup) != c.Network.AWS.Region {
		return nil, errors.New("unsafe release request")
	}
	for j, a := range c.addresses {
		if aws.ToString(a.AllocationId) == aws.ToString(in.AllocationId) {
			if a.AssociationId != nil {
				return nil, errors.New("still attached")
			}
			c.addresses = append(c.addresses[:j], c.addresses[j+1:]...)
			c.releases++
			if c.releaseAfter != nil {
				if err := c.releaseAfter(); err != nil {
					return nil, err
				}
			}
			return &ec2.ReleaseAddressOutput{}, nil
		}
	}
	return nil, errors.New("release repeated against missing address")
}
func terminateFixture(c *addressCloud) {
	for i := range c.Instances {
		c.Instances[i].State.Name = types.InstanceStateNameTerminated
	}
	for i := range c.addresses {
		c.addresses[i].AssociationId = nil
		c.addresses[i].NetworkInterfaceId = nil
		c.addresses[i].InstanceId = nil
	}
}
func ipamRelease(p Plan, c *addressCloud, s *memoryStore) (Record, error) {
	return ReleaseAddresses(context.Background(), p, testutil.Identity{Account: p.Network.AWS.AccountID}, c, s, "cleanup")
}

func TestIPAMReleaseRequiresEntireOwnedFleetTerminated(t *testing.T) {
	for _, change := range []string{"running", "stopped", "attached", "foreign", "missing-instance", "wrong-pool"} {
		t.Run(change, func(t *testing.T) {
			p, c, s := ipamSetup(t)
			if _, err := ipamRun(p, c, s); err != nil {
				t.Fatal(err)
			}
			terminateFixture(c)
			switch change {
			case "running":
				c.Instances[1].State.Name = types.InstanceStateNameRunning
			case "stopped":
				c.Instances[1].State.Name = types.InstanceStateNameStopped
			case "attached":
				c.addresses[1].AssociationId = aws.String("foreign")
			case "foreign":
				c.Instances[1].ClientToken = aws.String("other")
			case "missing-instance":
				c.Instances = c.Instances[:1]
			case "wrong-pool":
				c.addresses[1].PublicIpv4Pool = aws.String("other")
			}
			if _, err := ipamRelease(p, c, s); err == nil || c.releases != 0 {
				t.Fatal("unsafe release", err)
			}
		})
	}
}
func TestIPAMReleaseAndLostResponseReplay(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			p, c, s := ipamSetup(t)
			if _, err := ipamRun(p, c, s); err != nil {
				t.Fatal(err)
			}
			terminateFixture(c)
			if lost {
				c.releaseAfter = func() error { return errors.New("lost release response") }
				if _, err := ipamRelease(p, c, s); err == nil {
					t.Fatal("missing failure")
				}
				c.releaseAfter = nil
			}
			r, err := ipamRelease(p, c, s)
			if err != nil {
				t.Fatal(err)
			}
			if r.Phase != "addresses-released" || len(c.addresses) != 0 || c.releases != 2 || s.owner != "" {
				t.Fatal("incomplete release")
			}
			if _, err = ipamRelease(p, c, s); err != nil || c.releases != 2 {
				t.Fatal("release replay repeated request", err)
			}
			if _, err = ipamRun(p, c, s); err == nil || len(c.Requests) != 2 || c.allocations != 2 {
				t.Fatal("retired footprint was reprovisioned")
			}
		})
	}
}
