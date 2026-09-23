// Package inventory reads only explicitly scoped cloud resources.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/spec"
)

type EC2 interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}
type STS interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type Snapshot struct {
	APIVersion        string        `json:"apiVersion"`
	Kind              string        `json:"kind"`
	Network           spec.Metadata `json:"network"`
	Chain             spec.Chain    `json:"chain"`
	AccountID         string        `json:"accountId"`
	Region            string        `json:"region"`
	ObservedAt        time.Time     `json:"observedAt"`
	Source            string        `json:"source"`
	ApplicationHealth string        `json:"applicationHealth"`
	Instances         []Instance    `json:"instances"`
}

type Instance struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Role             string `json:"role"`
	State            string `json:"state"`
	Architecture     string `json:"architecture"`
	InstanceType     string `json:"instanceType"`
	AvailabilityZone string `json:"availabilityZone"`
	PrivateIP        string `json:"privateIp,omitempty"`
	PublicIP         string `json:"publicIp,omitempty"`
}

// Collect refuses the wrong account before requesting any inventory. A failed
// page returns an error, never a deceptively complete partial fleet.
func Collect(ctx context.Context, n spec.Network, identity STS, cloud EC2, now time.Time) (Snapshot, error) {
	if err := n.Validate(); err != nil {
		return Snapshot{}, err
	}
	caller, err := identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Snapshot{}, fmt.Errorf("verify AWS account: %w", err)
	}
	if caller == nil || aws.ToString(caller.Account) != n.AWS.AccountID {
		return Snapshot{}, errors.New("AWS credentials do not match the explicitly configured account")
	}
	s := Snapshot{APIVersion: spec.Version, Kind: "Inventory", Network: n.Metadata, Chain: n.Chain,
		AccountID: n.AWS.AccountID, Region: n.AWS.Region, ObservedAt: now.UTC(), Source: "aws-ec2",
		ApplicationHealth: "unknown", Instances: []Instance{}}
	input := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + n.AWS.NetworkTagKey), Values: []string{n.Metadata.Name}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}},
	}}
	seenIDs, seenPages := map[string]bool{}, map[string]bool{}
	for {
		out, err := cloud.DescribeInstances(ctx, input)
		if err != nil {
			return Snapshot{}, fmt.Errorf("read EC2 inventory: %w", err)
		}
		if out == nil {
			return Snapshot{}, errors.New("EC2 returned no response")
		}
		for _, reservation := range out.Reservations {
			for _, instance := range reservation.Instances {
				tags := map[string]string{}
				for _, tag := range instance.Tags {
					tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
				}
				if tags[n.AWS.NetworkTagKey] != n.Metadata.Name {
					return Snapshot{}, errors.New("EC2 returned an instance outside the requested network tag")
				}
				id := aws.ToString(instance.InstanceId)
				if id == "" || seenIDs[id] {
					return Snapshot{}, errors.New("EC2 returned a missing or duplicate instance identity; retry the snapshot")
				}
				seenIDs[id] = true
				state := "unknown"
				if instance.State != nil {
					state = string(instance.State.Name)
				}
				zone := ""
				if instance.Placement != nil {
					zone = aws.ToString(instance.Placement.AvailabilityZone)
				}
				role := tags["dashnet:role"]
				if role == "" {
					role = "unknown"
				}
				s.Instances = append(s.Instances, Instance{ID: id, Name: tags["Name"], Role: role,
					State: state, Architecture: string(instance.Architecture), InstanceType: string(instance.InstanceType),
					AvailabilityZone: zone, PrivateIP: aws.ToString(instance.PrivateIpAddress), PublicIP: aws.ToString(instance.PublicIpAddress)})
			}
		}
		token := aws.ToString(out.NextToken)
		if token == "" {
			break
		}
		if seenPages[token] {
			return Snapshot{}, errors.New("EC2 pagination repeated a token; snapshot is incomplete")
		}
		seenPages[token] = true
		input.NextToken = out.NextToken
	}
	sort.Slice(s.Instances, func(i, j int) bool { return s.Instances[i].ID < s.Instances[j].ID })
	return s, nil
}

func (s Snapshot) Validate() error {
	if s.APIVersion != spec.Version || s.Kind != "Inventory" || s.Source != "aws-ec2" || s.ObservedAt.IsZero() {
		return errors.New("invalid inventory version, kind, source, or timestamp")
	}
	if s.Network.Name == "" || s.Chain.Generation < 1 || (s.Network.Visibility != "private" && s.Network.Visibility != "public") {
		return errors.New("invalid inventory network identity or visibility")
	}
	if s.ApplicationHealth != "unknown" {
		return errors.New("EC2 inventory cannot assert application health")
	}
	return nil
}
