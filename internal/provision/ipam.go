package provision

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/spec"
)

// IPAM is deliberately separate so private-only operation needs no address APIs.
type IPAM interface {
	DescribeIpamPools(context.Context, *ec2.DescribeIpamPoolsInput, ...func(*ec2.Options)) (*ec2.DescribeIpamPoolsOutput, error)
	DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	AllocateAddress(context.Context, *ec2.AllocateAddressInput, ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error)
	AssociateAddress(context.Context, *ec2.AssociateAddressInput, ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error)
}

type AddressProgress struct {
	Phase         string    `json:"phase"` // allocating, allocated, associating, associated, releasing, released
	AttemptedAt   time.Time `json:"attemptedAt"`
	AllocationID  string    `json:"allocationId,omitempty"`
	PublicIP      string    `json:"publicIp,omitempty"`
	AssociationID string    `json:"associationId,omitempty"`
}

func validIPv4(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && a.Is4() && a.IsGlobalUnicast() && !a.IsPrivate()
}

func validateAddress(p Plan, n NodeProgress) error {
	a := n.Address
	if a == nil {
		return nil
	}
	if !p.Network.AWS.Provision.PublicIPv4 || p.Network.AWS.Provision.IPAMPoolID == "" || n.Phase != "present" || a.AttemptedAt.IsZero() {
		return errors.New("address journal has no matching IPAM launch")
	}
	switch a.Phase {
	case "allocating":
		if a.AllocationID != "" || a.PublicIP != "" || a.AssociationID != "" {
			return errors.New("invalid allocation intent")
		}
	case "allocated", "associating", "associated", "releasing", "released":
		if a.AllocationID == "" || !validIPv4(a.PublicIP) {
			return errors.New("invalid allocated address journal")
		}
		if a.Phase == "associated" && a.AssociationID == "" {
			return errors.New("associated address has no association identity")
		}
	default:
		return errors.New("invalid address phase")
	}
	return nil
}

func verifyPool(ctx context.Context, n spec.Network, c IPAM) error {
	id := n.AWS.Provision.IPAMPoolID
	out, err := c.DescribeIpamPools(ctx, &ec2.DescribeIpamPoolsInput{IpamPoolIds: []string{id}})
	if err != nil {
		return fmt.Errorf("describe IPAM pool: %w", err)
	}
	if out == nil || len(out.IpamPools) != 1 || aws.ToString(out.NextToken) != "" {
		return errors.New("expected one complete IPAM pool response")
	}
	p := out.IpamPools[0]
	if aws.ToString(p.IpamPoolId) != id || aws.ToString(p.OwnerId) != n.AWS.AccountID || aws.ToString(p.Locale) != n.AWS.Region || string(p.State) != "create-complete" || string(p.AddressFamily) != "ipv4" || string(p.IpamScopeType) != "public" || string(p.PublicIpSource) != "byoip" || string(p.AwsService) != "ec2" {
		return errors.New("IPAM pool must be owned, ready, public BYOIP IPv4 for EC2 in the target region")
	}
	return nil
}

func primaryENI(i types.Instance) (string, error) {
	id := ""
	for _, ni := range i.NetworkInterfaces {
		if ni.Attachment != nil && ni.Attachment.DeviceIndex != nil && aws.ToInt32(ni.Attachment.DeviceIndex) == 0 {
			if id != "" || aws.ToString(ni.NetworkInterfaceId) == "" {
				return "", errors.New("ambiguous primary network interface")
			}
			id = aws.ToString(ni.NetworkInterfaceId)
		}
	}
	if id == "" {
		return "", errors.New("missing primary network interface")
	}
	return id, nil
}

func discoverAddresses(ctx context.Context, p Plan, c IPAM, r Record, live map[string]types.Instance) (map[string]types.Address, error) {
	out, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: []types.Filter{{Name: aws.String("tag:" + p.Network.AWS.NetworkTagKey), Values: []string{p.Network.Metadata.Name}}}})
	if err != nil {
		return nil, fmt.Errorf("reconcile IPAM addresses: %w", err)
	}
	if out == nil {
		return nil, errors.New("empty address response")
	}
	targets := map[string]Target{}
	for _, t := range p.Targets {
		targets[t.Name] = t
	}
	result := map[string]types.Address{}
	ids := map[string]bool{}
	for _, a := range out.Addresses {
		tags := map[string]string{}
		for _, tag := range a.Tags {
			k := aws.ToString(tag.Key)
			if _, ok := tags[k]; ok {
				return nil, errors.New("duplicate address tag")
			}
			tags[k] = aws.ToString(tag.Value)
		}
		t, ok := targets[tags["dashnet:node"]]
		if !ok {
			return nil, errors.New("unexpected address in network scope; no adoption")
		}
		for k, v := range p.Tags(t) {
			if tags[k] != v {
				return nil, fmt.Errorf("address ownership mismatch for %s", t.Name)
			}
		}
		id, ip := aws.ToString(a.AllocationId), aws.ToString(a.PublicIp)
		if _, duplicate := result[t.Name]; duplicate || id == "" || ids[id] || !validIPv4(ip) || aws.ToString(a.PublicIpv4Pool) != p.Network.AWS.Provision.IPAMPoolID || aws.ToString(a.NetworkBorderGroup) != p.Network.AWS.Region || string(a.Domain) != "vpc" {
			return nil, fmt.Errorf("duplicate or wrong-pool address for %s", t.Name)
		}
		ids[id] = true
		n := r.Nodes[t.Name]
		if n.Address == nil {
			return nil, fmt.Errorf("address for %s exists without journaled intent; no adoption", t.Name)
		}
		if n.Address.AllocationID != "" && (n.Address.AllocationID != id || n.Address.PublicIP != ip) {
			return nil, fmt.Errorf("address identity changed for %s", t.Name)
		}
		if aws.ToString(a.AssociationId) != "" || aws.ToString(a.NetworkInterfaceId) != "" || aws.ToString(a.InstanceId) != "" {
			i, ok := live[t.Name]
			if !ok {
				return nil, fmt.Errorf("address attached without live owned instance %s", t.Name)
			}
			eni, err := primaryENI(i)
			if err != nil {
				return nil, err
			}
			if aws.ToString(a.AssociationId) == "" || aws.ToString(a.NetworkInterfaceId) != eni || aws.ToString(a.InstanceId) != n.InstanceID || (n.Address.AssociationID != "" && n.Address.AssociationID != aws.ToString(a.AssociationId)) {
				return nil, fmt.Errorf("address association drift for %s; no reassociation", t.Name)
			}
		} else if n.Address.Phase == "associated" {
			return nil, fmt.Errorf("address unexpectedly detached for %s", t.Name)
		}
		result[t.Name] = a
	}
	for name, n := range r.Nodes {
		if n.Address != nil && n.Address.AllocationID != "" {
			if _, ok := result[name]; !ok {
				return nil, fmt.Errorf("recorded address not visible for %s; no replacement", name)
			}
		}
	}
	return result, nil
}

func ensureAddresses(ctx context.Context, p Plan, c IPAM, r *Record, live map[string]types.Instance, save func() error, report func(string)) error {
	addresses, err := discoverAddresses(ctx, p, c, *r, live)
	if err != nil {
		return err
	}
	// Check every primary interface and unexpected public address before allocation.
	for _, t := range p.Targets {
		i := live[t.Name]
		if _, err := primaryENI(i); err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
		if ip := aws.ToString(i.PublicIpAddress); ip != "" && ip != aws.ToString(addresses[t.Name].PublicIp) {
			return fmt.Errorf("unexpected public address on %s; no replacement", t.Name)
		}
	}
	for _, t := range p.Targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := r.Nodes[t.Name]
		a, found := addresses[t.Name]
		if !found {
			// AllocateAddress has no client token. Neither the runner nor the AWS SDK
			// may blindly repeat it after a timeout, throttling or lost response.
			if n.Address != nil {
				return fmt.Errorf("IPAM allocation outcome unknown for %s; reconcile later, no automatic reallocation", t.Name)
			}
			n.Address = &AddressProgress{Phase: "allocating", AttemptedAt: time.Now().UTC()}
			r.Nodes[t.Name] = n
			if err := save(); err != nil {
				return err
			}
			tags := []types.Tag{}
			for k, v := range p.Tags(t) {
				tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
			}
			sort.Slice(tags, func(i, j int) bool { return *tags[i].Key < *tags[j].Key })
			report("allocating IPAM address for " + t.Name)
			out, err := c.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: types.DomainTypeVpc, IpamPoolId: aws.String(p.Network.AWS.Provision.IPAMPoolID), NetworkBorderGroup: aws.String(p.Network.AWS.Region), TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeElasticIp, Tags: tags}}}, func(o *ec2.Options) { o.RetryMaxAttempts = 1 })
			if err != nil {
				return fmt.Errorf("allocate IPAM address for %s (outcome may be uncertain): %w", t.Name, err)
			}
			if out == nil || aws.ToString(out.AllocationId) == "" || !validIPv4(aws.ToString(out.PublicIp)) {
				return errors.New("invalid allocation reply; reconcile tagged intent before retrying")
			}
			n.Address.AllocationID, n.Address.PublicIP, n.Address.Phase = *out.AllocationId, *out.PublicIp, "allocated"
			r.Nodes[t.Name] = n
			if err := save(); err != nil {
				return err
			}
			addresses, err = discoverAddresses(ctx, p, c, *r, live)
			if err != nil {
				return err
			}
			a, found = addresses[t.Name]
			if !found {
				return errors.New("allocated address not yet visible; resume reconciliation")
			}
		}
		// Reconcile a successful allocation whose reply/checkpoint was lost.
		if n.Address.Phase == "allocating" {
			n.Address.AllocationID, n.Address.PublicIP, n.Address.Phase = aws.ToString(a.AllocationId), aws.ToString(a.PublicIp), "allocated"
			r.Nodes[t.Name] = n
			if err := save(); err != nil {
				return err
			}
		}
		if aws.ToString(a.AssociationId) == "" {
			if n.Address.Phase == "associating" {
				return fmt.Errorf("association outcome unknown for %s; no automatic reassociation", t.Name)
			}
			n.Address.Phase = "associating"
			r.Nodes[t.Name] = n
			if err := save(); err != nil {
				return err
			}
			report("associating IPAM address for " + t.Name)
			_, err := c.AssociateAddress(ctx, &ec2.AssociateAddressInput{AllocationId: a.AllocationId, InstanceId: aws.String(n.InstanceID), AllowReassociation: aws.Bool(false)}, func(o *ec2.Options) { o.RetryMaxAttempts = 1 })
			if err != nil {
				return fmt.Errorf("associate IPAM address for %s: %w", t.Name, err)
			}
			addresses, err = discoverAddresses(ctx, p, c, *r, live)
			if err != nil {
				return err
			}
			a = addresses[t.Name]
			if aws.ToString(a.AssociationId) == "" {
				return fmt.Errorf("association not yet visible for %s; resume reconciliation", t.Name)
			}
		}
		n.Address.Phase, n.Address.AssociationID = "associated", aws.ToString(a.AssociationId)
		r.Nodes[t.Name] = n
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}
