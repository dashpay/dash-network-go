package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/spec"
)

func ProvisionNetwork(t testing.TB) spec.Network {
	n := Network(t)
	n.Nodes = n.Nodes[:1]
	n.Nodes[0].Count = 2
	n.AWS.Provision = &spec.Provision{StateTable: "dashnet-operations", VPCID: "vpc-00000001", SubnetID: "subnet-00000001", SecurityGroupIDs: []string{"sg-00000001"}, KeyName: "emergency", RootVolumeGiB: 100, AMIs: map[string]spec.AMI{"arm64": {ID: "ami-00000001", OwnerID: n.AWS.AccountID}}}
	return n
}

type Identity struct{ Account string }

func (i Identity) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(i.Account)}, nil
}

// Cloud is an in-memory EC2 fake. Hooks simulate request ambiguity and eventual
// consistency. It never reaches credentials, real endpoints, or paid resources.
type Cloud struct {
	Network    spec.Network
	Mu         sync.Mutex
	Instances  []types.Instance
	Requests   []*ec2.RunInstancesInput
	Reads      int
	BeforeRun  func(*ec2.RunInstancesInput) error
	AfterRun   func() error
	Describe   func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)
	ImageHook  func(*types.Image)
	SubnetHook func(*types.Subnet)
	TypeHook   func(*types.InstanceTypeInfo)
}

func (c *Cloud) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	s := types.Subnet{SubnetId: aws.String(c.Network.AWS.Provision.SubnetID), VpcId: aws.String(c.Network.AWS.Provision.VPCID), OwnerId: aws.String(c.Network.AWS.AccountID), State: types.SubnetStateAvailable}
	if c.SubnetHook != nil {
		c.SubnetHook(&s)
	}
	return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{s}}, nil
}
func (c *Cloud) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, g := range c.Network.AWS.Provision.SecurityGroupIDs {
		out.SecurityGroups = append(out.SecurityGroups, types.SecurityGroup{GroupId: aws.String(g), VpcId: aws.String(c.Network.AWS.Provision.VPCID), OwnerId: aws.String(c.Network.AWS.AccountID)})
	}
	return out, nil
}
func (c *Cloud) DescribeKeyPairs(context.Context, *ec2.DescribeKeyPairsInput, ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error) {
	return &ec2.DescribeKeyPairsOutput{KeyPairs: []types.KeyPairInfo{{KeyName: aws.String(c.Network.AWS.Provision.KeyName)}}}, nil
}
func (c *Cloud) DescribeImages(_ context.Context, input *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	var image types.Image
	for a, ami := range c.Network.AWS.Provision.AMIs {
		if input.ImageIds[0] != ami.ID {
			continue
		}
		arch := types.ArchitectureValuesArm64
		if a == "amd64" {
			arch = types.ArchitectureValuesX8664
		}
		image = types.Image{ImageId: aws.String(ami.ID), OwnerId: aws.String(ami.OwnerID), Architecture: arch, RootDeviceName: aws.String("/dev/sda1"), RootDeviceType: types.DeviceTypeEbs, VirtualizationType: types.VirtualizationTypeHvm, State: types.ImageStateAvailable, BlockDeviceMappings: []types.BlockDeviceMapping{{DeviceName: aws.String("/dev/sda1"), Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(8)}}}}
	}
	if c.ImageHook != nil {
		c.ImageHook(&image)
	}
	return &ec2.DescribeImagesOutput{Images: []types.Image{image}}, nil
}
func (c *Cloud) DescribeInstanceTypes(_ context.Context, input *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	info := types.InstanceTypeInfo{InstanceType: input.InstanceTypes[0], ProcessorInfo: &types.ProcessorInfo{SupportedArchitectures: []types.ArchitectureType{types.ArchitectureTypeArm64, types.ArchitectureTypeX8664}}, SupportedRootDeviceTypes: []types.RootDeviceType{types.RootDeviceTypeEbs}, SupportedVirtualizationTypes: []types.VirtualizationType{types.VirtualizationTypeHvm}}
	if c.TypeHook != nil {
		c.TypeHook(&info)
	}
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []types.InstanceTypeInfo{info}}, nil
}
func (c *Cloud) DescribeInstances(_ context.Context, input *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Reads++
	if c.Describe != nil {
		return c.Describe(input)
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{OwnerId: aws.String(c.Network.AWS.AccountID), Instances: append([]types.Instance(nil), c.Instances...)}}}, nil
}
func (c *Cloud) RunInstances(_ context.Context, input *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	if c.BeforeRun != nil {
		if err := c.BeforeRun(input); err != nil {
			return nil, err
		}
	}
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.Requests = append(c.Requests, input)
	for _, i := range c.Instances {
		if aws.ToString(i.ClientToken) == aws.ToString(input.ClientToken) {
			return nil, errors.New("runner unexpectedly repeated an EC2 request")
		}
	}
	architecture := types.ArchitectureValuesArm64
	for a, img := range c.Network.AWS.Provision.AMIs {
		if img.ID == aws.ToString(input.ImageId) && a == "amd64" {
			architecture = types.ArchitectureValuesX8664
		}
	}
	id := fmt.Sprintf("i-%08x", len(c.Instances)+1)
	instance := types.Instance{InstanceId: aws.String(id), ClientToken: input.ClientToken, ImageId: input.ImageId, InstanceType: input.InstanceType, Architecture: architecture, SubnetId: input.NetworkInterfaces[0].SubnetId, VpcId: aws.String(c.Network.AWS.Provision.VPCID), KeyName: input.KeyName, MetadataOptions: &types.InstanceMetadataOptionsResponse{HttpTokens: types.HttpTokensStateRequired}, State: &types.InstanceState{Name: types.InstanceStateNameRunning}}
	for _, g := range input.NetworkInterfaces[0].Groups {
		instance.SecurityGroups = append(instance.SecurityGroups, types.GroupIdentifier{GroupId: aws.String(g)})
	}
	for _, tags := range input.TagSpecifications {
		if tags.ResourceType == types.ResourceTypeInstance {
			instance.Tags = tags.Tags
		}
	}
	c.Instances = append(c.Instances, instance)
	if c.AfterRun != nil {
		if err := c.AfterRun(); err != nil {
			return nil, err
		}
	}
	return &ec2.RunInstancesOutput{Instances: []types.Instance{instance}}, nil
}
