package provision

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/spec"
)

type EC2 interface {
	inventory.EC2
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeKeyPairs(context.Context, *ec2.DescribeKeyPairsInput, ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error)
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
}

func VerifyAccount(ctx context.Context, expected string, identity inventory.STS) error {
	caller, err := identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("verify AWS account: %w", err)
	}
	if caller == nil || aws.ToString(caller.Account) != expected {
		return errors.New("AWS credentials do not match the configured account")
	}
	return nil
}

func arch(a string) types.ArchitectureType {
	if a == "amd64" {
		return types.ArchitectureTypeX8664
	}
	return types.ArchitectureTypeArm64
}

// Prepare is read-only. Execute repeats these checks against the immutable plan.
// It does not claim to verify routes, SSH trust, OS preparation, or chain health.
func Prepare(ctx context.Context, n spec.Network, identity inventory.STS, cloud EC2) (Plan, error) {
	if err := n.Validate(); err != nil {
		return Plan{}, err
	}
	if err := n.ValidateProvision(); err != nil {
		return Plan{}, err
	}
	if err := VerifyAccount(ctx, n.AWS.AccountID, identity); err != nil {
		return Plan{}, err
	}
	cfg := n.AWS.Provision
	subnets, err := cloud.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{cfg.SubnetID}})
	if err != nil {
		return Plan{}, fmt.Errorf("describe subnet: %w", err)
	}
	if subnets == nil || len(subnets.Subnets) != 1 {
		return Plan{}, errors.New("expected exactly one subnet")
	}
	subnet := subnets.Subnets[0]
	if aws.ToString(subnet.SubnetId) != cfg.SubnetID || aws.ToString(subnet.VpcId) != cfg.VPCID || aws.ToString(subnet.OwnerId) != n.AWS.AccountID || subnet.State != types.SubnetStateAvailable {
		return Plan{}, errors.New("subnet is unavailable or does not belong to the configured VPC/account")
	}
	groups, err := cloud.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: cfg.SecurityGroupIDs})
	if err != nil {
		return Plan{}, fmt.Errorf("describe security groups: %w", err)
	}
	if groups == nil || len(groups.SecurityGroups) != len(cfg.SecurityGroupIDs) {
		return Plan{}, errors.New("incomplete security group response")
	}
	seen := map[string]bool{}
	for _, g := range groups.SecurityGroups {
		id := aws.ToString(g.GroupId)
		if seen[id] || !slices.Contains(cfg.SecurityGroupIDs, id) || aws.ToString(g.OwnerId) != n.AWS.AccountID || aws.ToString(g.VpcId) != cfg.VPCID {
			return Plan{}, errors.New("security group scope mismatch")
		}
		seen[id] = true
	}
	keys, err := cloud.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{KeyNames: []string{cfg.KeyName}})
	if err != nil {
		return Plan{}, fmt.Errorf("describe emergency SSH key pair: %w", err)
	}
	if keys == nil || len(keys.KeyPairs) != 1 || aws.ToString(keys.KeyPairs[0].KeyName) != cfg.KeyName {
		return Plan{}, errors.New("expected configured SSH key pair")
	}
	roots := map[string]string{}
	for _, a := range n.Architectures() {
		requested := cfg.AMIs[a]
		images, err := cloud.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{requested.ID}, Owners: []string{requested.OwnerID}})
		if err != nil {
			return Plan{}, fmt.Errorf("describe %s AMI: %w", a, err)
		}
		if images == nil || len(images.Images) != 1 {
			return Plan{}, fmt.Errorf("expected one %s AMI", a)
		}
		img := images.Images[0]
		root := aws.ToString(img.RootDeviceName)
		if aws.ToString(img.ImageId) != requested.ID || aws.ToString(img.OwnerId) != requested.OwnerID || string(img.Architecture) != string(arch(a)) || img.State != types.ImageStateAvailable || img.RootDeviceType != types.DeviceTypeEbs || img.VirtualizationType != types.VirtualizationTypeHvm || img.Platform != "" || len(img.ProductCodes) != 0 || root == "" {
			return Plan{}, fmt.Errorf("%s AMI must be available, owner-pinned, architecture-matched, HVM Linux/EBS, with no marketplace product codes", a)
		}
		if err := validateImageDisks(img, cfg.RootVolumeGiB); err != nil {
			return Plan{}, fmt.Errorf("%s AMI: %w", a, err)
		}
		roots[a] = root
	}
	checked := map[string]bool{}
	for _, g := range n.Nodes {
		key := g.InstanceType + "/" + g.Architecture
		if checked[key] {
			continue
		}
		checked[key] = true
		out, err := cloud.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{types.InstanceType(g.InstanceType)}})
		if err != nil {
			return Plan{}, fmt.Errorf("describe instance type %s: %w", g.InstanceType, err)
		}
		if out == nil || len(out.InstanceTypes) != 1 {
			return Plan{}, errors.New("incomplete instance type response")
		}
		info := out.InstanceTypes[0]
		if string(info.InstanceType) != g.InstanceType || info.ProcessorInfo == nil || !slices.Contains(info.ProcessorInfo.SupportedArchitectures, arch(g.Architecture)) || !slices.Contains(info.SupportedRootDeviceTypes, types.RootDeviceTypeEbs) || !slices.Contains(info.SupportedVirtualizationTypes, types.VirtualizationTypeHvm) {
			return Plan{}, fmt.Errorf("instance type %s does not support requested architecture/EBS/HVM", g.InstanceType)
		}
	}
	p := build(n, roots)
	return p, p.Validate()
}

var ephemeralDevice = regexp.MustCompile(`^ephemeral[0-9]+$`)

// Canonical Ubuntu AMIs include ephemeral instance-store declarations even on
// EBS-only instance types. They do not create additional billable EBS volumes.
// Reject every extra EBS or ambiguous mapping, irrespective of its position.
func validateImageDisks(img types.Image, requestedGiB int32) error {
	root := aws.ToString(img.RootDeviceName)
	roots := 0
	devices, virtuals := map[string]bool{}, map[string]bool{}
	for _, mapping := range img.BlockDeviceMappings {
		device, virtual := aws.ToString(mapping.DeviceName), aws.ToString(mapping.VirtualName)
		if device == "" || devices[device] || mapping.NoDevice != nil {
			return errors.New("invalid, duplicate or suppressed block-device mapping")
		}
		devices[device] = true
		if device == root {
			if mapping.Ebs == nil || virtual != "" || aws.ToInt32(mapping.Ebs.VolumeSize) < 1 || aws.ToInt32(mapping.Ebs.VolumeSize) > requestedGiB {
				return errors.New("root must be one EBS disk no larger than rootVolumeGiB")
			}
			roots++
		} else {
			if mapping.Ebs != nil || !ephemeralDevice.MatchString(virtual) || virtuals[virtual] {
				return errors.New("extra disks must be unique ephemeral instance-store mappings, never extra EBS volumes")
			}
			virtuals[virtual] = true
		}
	}
	if roots != 1 {
		return errors.New("exactly one EBS root mapping is required")
	}
	return nil
}
