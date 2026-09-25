package provision

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func launch(ctx context.Context, p Plan, t Target, c EC2) (string, error) {
	cfg := p.Network.AWS.Provision
	tags := []types.Tag{}
	for k, v := range p.Tags(t) {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	sort.Slice(tags, func(i, j int) bool { return *tags[i].Key < *tags[j].Key })
	out, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: aws.String(t.AMI), InstanceType: types.InstanceType(t.InstanceType), MinCount: aws.Int32(1), MaxCount: aws.Int32(1), ClientToken: aws.String(p.Token(t)), KeyName: aws.String(cfg.KeyName),
		// A specified subnet gives the idempotency token a stable AZ scope.
		NetworkInterfaces:   []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), SubnetId: aws.String(cfg.SubnetID), Groups: cfg.SecurityGroupIDs, AssociatePublicIpAddress: aws.Bool(false), DeleteOnTermination: aws.Bool(true)}},
		BlockDeviceMappings: []types.BlockDeviceMapping{{DeviceName: aws.String(t.RootDevice), Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(cfg.RootVolumeGiB), VolumeType: types.VolumeTypeGp3, Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(false)}}},
		MetadataOptions:     &types.InstanceMetadataOptionsRequest{HttpTokens: types.HttpTokensStateRequired, HttpEndpoint: types.InstanceMetadataEndpointStateEnabled, HttpPutResponseHopLimit: aws.Int32(1)},
		TagSpecifications:   []types.TagSpecification{{ResourceType: types.ResourceTypeInstance, Tags: tags}, {ResourceType: types.ResourceTypeVolume, Tags: tags}},
	})
	if err != nil {
		return "", err
	}
	if out == nil || len(out.Instances) != 1 || aws.ToString(out.Instances[0].InstanceId) == "" {
		return "", errors.New("expected exactly one launched instance identity")
	}
	return aws.ToString(out.Instances[0].InstanceId), nil
}

func discover(ctx context.Context, p Plan, c EC2) (map[string]types.Instance, error) {
	input := &ec2.DescribeInstancesInput{Filters: []types.Filter{{Name: aws.String("tag:" + p.Network.AWS.NetworkTagKey), Values: []string{p.Network.Metadata.Name}}}}
	targets := map[string]Target{}
	for _, t := range p.Targets {
		targets[t.Name] = t
	}
	result := map[string]types.Instance{}
	pages := map[string]bool{}
	ids := map[string]bool{}
	for {
		out, err := c.DescribeInstances(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("reconcile EC2 scope: %w", err)
		}
		if out == nil {
			return nil, errors.New("empty EC2 discovery response")
		}
		for _, reservation := range out.Reservations {
			if aws.ToString(reservation.OwnerId) != p.Network.AWS.AccountID {
				return nil, errors.New("EC2 reservation account mismatch")
			}
			for _, instance := range reservation.Instances {
				tags := map[string]string{}
				for _, tag := range instance.Tags {
					k := aws.ToString(tag.Key)
					if _, ok := tags[k]; ok {
						return nil, errors.New("duplicate ownership tag")
					}
					tags[k] = aws.ToString(tag.Value)
				}
				t, ok := targets[tags["dashnet:node"]]
				if !ok {
					return nil, errors.New("network tag includes an unmanaged or unexpected target; refusing legacy adoption")
				}
				for k, v := range p.Tags(t) {
					if tags[k] != v {
						return nil, fmt.Errorf("ownership mismatch for target %s tag %s", t.Name, k)
					}
				}
				id := aws.ToString(instance.InstanceId)
				if id == "" || ids[id] {
					return nil, errors.New("missing or duplicate EC2 identity")
				}
				ids[id] = true
				if _, ok := result[t.Name]; ok {
					return nil, fmt.Errorf("multiple instances for target %s", t.Name)
				}
				if aws.ToString(instance.ClientToken) != p.Token(t) || aws.ToString(instance.ImageId) != t.AMI || string(instance.InstanceType) != t.InstanceType || string(instance.Architecture) != string(arch(t.Architecture)) || aws.ToString(instance.SubnetId) != p.Network.AWS.Provision.SubnetID || aws.ToString(instance.VpcId) != p.Network.AWS.Provision.VPCID || aws.ToString(instance.KeyName) != p.Network.AWS.Provision.KeyName {
					return nil, fmt.Errorf("launch identity or placement drift for target %s", t.Name)
				}
				sg := map[string]bool{}
				for _, g := range instance.SecurityGroups {
					sg[aws.ToString(g.GroupId)] = true
				}
				if len(sg) != len(p.Network.AWS.Provision.SecurityGroupIDs) {
					return nil, fmt.Errorf("security group drift for %s", t.Name)
				}
				for _, g := range p.Network.AWS.Provision.SecurityGroupIDs {
					if !sg[g] {
						return nil, fmt.Errorf("security group drift for %s", t.Name)
					}
				}
				if instance.MetadataOptions == nil || instance.MetadataOptions.HttpTokens != types.HttpTokensStateRequired {
					return nil, fmt.Errorf("IMDSv2 drift for %s", t.Name)
				}
				if instance.State == nil || (instance.State.Name != types.InstanceStateNamePending && instance.State.Name != types.InstanceStateNameRunning) {
					return nil, fmt.Errorf("target %s is not pending/running; no implicit restart or replacement", t.Name)
				}
				result[t.Name] = instance
			}
		}
		token := aws.ToString(out.NextToken)
		if token == "" {
			return result, nil
		}
		if pages[token] {
			return nil, errors.New("EC2 pagination repeated a token; refusing partial scope")
		}
		pages[token] = true
		input.NextToken = out.NextToken
	}
}

func reconcile(p Plan, r *Record, live map[string]types.Instance, waitForVisibility bool) error {
	for _, t := range p.Targets {
		node := r.Nodes[t.Name]
		instance, found := live[t.Name]
		if !found {
			if waitForVisibility {
				node.EC2State = "unknown"
				r.Nodes[t.Name] = node
				continue
			}
			if node.Phase != "pending" {
				node.EC2State = "unknown"
				node.ObservedAt = time.Now().UTC()
				r.Nodes[t.Name] = node
				return fmt.Errorf("target %s has recorded launch intent but no visible instance; outcome unknown, no automatic relaunch (retry reconciliation after EC2 propagation)", t.Name)
			}
			continue
		}
		if node.Phase == "pending" {
			return fmt.Errorf("target %s exists without journaled launch intent; refusing adoption", t.Name)
		}
		id := aws.ToString(instance.InstanceId)
		if node.InstanceID != "" && node.InstanceID != id {
			return fmt.Errorf("target %s instance identity changed", t.Name)
		}
		node.InstanceID = id
		node.Phase = "present"
		node.EC2State = string(instance.State.Name)
		node.ObservedAt = time.Now().UTC()
		r.Nodes[t.Name] = node
	}
	return nil
}

// Footprint is suitable for a terminal summary, not a public status projection.
func (p Plan) Footprint() string {
	cfg := p.Network.AWS.Provision
	return fmt.Sprintf("%s: %d on-demand EC2 instances, %d GiB gp3 root storage, public IPv4=%s, IPAM pool=%s; existing subnet %s; root disks and IPAM Elastic IPs retained on termination", p.Network.Metadata.Name, len(p.Targets), int64(len(p.Targets))*int64(cfg.RootVolumeGiB), strconv.FormatBool(cfg.PublicIPv4), cfg.IPAMPoolID, cfg.SubnetID)
}
