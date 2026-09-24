package managed

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/provision"
)

func VerifyCloud(ctx context.Context, f Fleet, identity inventory.STS, cloud inventory.EC2) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := provision.VerifyAccount(ctx, f.AccountID, identity); err != nil {
		return err
	}
	expected := map[string]Target{}
	ids := []string{}
	for _, t := range f.Targets {
		expected[t.InstanceID] = t
		ids = append(ids, t.InstanceID)
	}
	found := map[string]bool{}
	input := &ec2.DescribeInstancesInput{InstanceIds: ids}
	pages := map[string]bool{}
	for {
		out, err := cloud.DescribeInstances(ctx, input)
		if err != nil {
			return err
		}
		if out == nil {
			return errors.New("empty EC2 scope response")
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				id := aws.ToString(i.InstanceId)
				t, ok := expected[id]
				if !ok || found[id] {
					return errors.New("EC2 returned unexpected/duplicate target")
				}
				found[id] = true
				tag := ""
				for _, v := range i.Tags {
					if aws.ToString(v.Key) == f.NetworkTagKey {
						tag = aws.ToString(v.Value)
					}
				}
				arch := string(i.Architecture)
				if arch == "x86_64" {
					arch = "amd64"
				}
				if tag != f.Metadata.Name || i.State == nil || string(i.State.Name) != "running" || arch != t.Architecture || (aws.ToString(i.PublicIpAddress) != t.Address && aws.ToString(i.PrivateIpAddress) != t.Address) {
					return fmt.Errorf("%s: live EC2 scope/address/state mismatch", t.Name)
				}
			}
		}
		next := aws.ToString(out.NextToken)
		if next == "" {
			break
		}
		if pages[next] {
			return errors.New("repeated EC2 page")
		}
		pages[next] = true
		input.NextToken = out.NextToken
	}
	if len(found) != len(expected) {
		return errors.New("EC2 lost an intended target")
	}
	return nil
}
