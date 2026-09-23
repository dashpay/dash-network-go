package inventory_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

type identity struct{ account string }

func (i identity) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(i.account)}, nil
}

type cloud struct {
	calls int
	fn    func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)
}

func (c *cloud) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	c.calls++
	return c.fn(in)
}
func instance(id, network string) types.Instance {
	return types.Instance{InstanceId: aws.String(id), State: &types.InstanceState{Name: types.InstanceStateNameRunning}, Tags: []types.Tag{{Key: aws.String("DashNetwork"), Value: aws.String(network)}}}
}

func TestAccountMismatchNeverListsInstances(t *testing.T) {
	n := testutil.Network(t)
	c := &cloud{}
	if _, err := inventory.Collect(context.Background(), n, identity{"999999999999"}, c, time.Now()); err == nil {
		t.Fatal("wrong account accepted")
	}
	if c.calls != 0 {
		t.Fatal("listed before account verification")
	}
}

func TestPaginationAndExactScope(t *testing.T) {
	n := testutil.Network(t)
	c := &cloud{fn: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		if aws.ToString(in.Filters[0].Name) != "tag:DashNetwork" || !reflect.DeepEqual(in.Filters[0].Values, []string{n.Metadata.Name}) {
			t.Fatal("network filter missing")
		}
		if in.NextToken == nil {
			return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance("i-b", n.Metadata.Name)}}}, NextToken: aws.String("page2")}, nil
		}
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance("i-a", n.Metadata.Name)}}}}, nil
	}}
	s, err := inventory.Collect(context.Background(), n, identity{n.AWS.AccountID}, c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 2 || len(s.Instances) != 2 || s.Instances[0].ID != "i-a" {
		t.Fatal("pagination/sorting incomplete")
	}
	if s.ApplicationHealth != "unknown" {
		t.Fatal("running EC2 interpreted as healthy application")
	}
}

func TestFailureCannotMasqueradeAsCompleteInventory(t *testing.T) {
	n := testutil.Network(t)
	for _, mode := range []string{"page-fails", "wrong-network", "repeated-token", "duplicate-id"} {
		t.Run(mode, func(t *testing.T) {
			c := &cloud{fn: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
				if mode == "page-fails" && in.NextToken != nil {
					return nil, errors.New("unavailable")
				}
				network := n.Metadata.Name
				if mode == "wrong-network" {
					network = "other"
				}
				id := "i-a"
				if mode == "repeated-token" && in.NextToken != nil {
					id = "i-b"
				}
				return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance(id, network)}}}, NextToken: aws.String("again")}, nil
			}}
			s, err := inventory.Collect(context.Background(), n, identity{n.AWS.AccountID}, c, time.Now())
			if err == nil || len(s.Instances) != 0 {
				t.Fatal("incomplete inventory reported as success")
			}
		})
	}
}
