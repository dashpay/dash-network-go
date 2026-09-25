package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/dashpay/dash-network-go/internal/inventory"
)

type addressReleaser interface {
	IPAM
	ReleaseAddress(context.Context, *ec2.ReleaseAddressInput, ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error)
}

// A missing tag-search result is not proof of deletion: tags may have changed.
// Require AWS to report the exact allocation ID as gone. Permissions, transport
// failures and empty/malformed responses remain unresolved.
func verifyAddressReleased(ctx context.Context, c IPAM, id string) error {
	_, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []string{id}})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidAllocationID.NotFound" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify exact released allocation: %w", err)
	}
	return errors.New("released allocation not proven absent by ID; inspect before resuming")
}

// ReleaseAddresses never terminates or disassociates. Every original instance
// must still be visible as terminated, owned by this plan; every EIP must already
// be detached. An old AWS tombstone disappearing requires operator investigation.
func ReleaseAddresses(ctx context.Context, p Plan, identity inventory.STS, cloud EC2, store Store, owner string) (result Record, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	if !p.Network.AWS.Provision.PublicIPv4 || p.Network.AWS.Provision.IPAMPoolID == "" || owner == "" {
		return result, errors.New("an IPAM provision plan and runner identity are required")
	}
	c, ok := cloud.(addressReleaser)
	if !ok {
		return result, errors.New("EC2 client cannot release IPAM addresses")
	}
	if err = VerifyAccount(ctx, p.Network.AWS.AccountID, identity); err != nil {
		return
	}
	r, err := store.Acquire(ctx, p, owner)
	if err != nil {
		return result, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err != nil && r.Validate(p) == nil && r.Phase != "addresses-released" {
			r.Phase = "interrupted"
			r.LastError = err.Error()
			r.UpdatedAt = time.Now().UTC()
			r.Revision++
			if e := store.Save(cleanup, r, owner); e != nil {
				err = errors.Join(err, e)
			}
		}
		if e := store.Release(cleanup, p, owner); e != nil {
			err = errors.Join(err, e)
		}
		result = r
	}()
	if err = r.Validate(p); err != nil {
		return
	}
	if r.Phase == "addresses-released" {
		return
	}
	r.LastRunner = owner
	save := func() error { r.Revision++; r.UpdatedAt = time.Now().UTC(); return store.Save(ctx, r, owner) }
	ids := []string{}
	for _, t := range p.Targets {
		n := r.Nodes[t.Name]
		if n.Phase != "present" || n.InstanceID == "" {
			return result, fmt.Errorf("%s has unresolved instance identity; reconcile before cleanup", t.Name)
		}
		ids = append(ids, n.InstanceID)
	}
	out, err := cloud.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		return result, err
	}
	if out == nil || aws.ToString(out.NextToken) != "" {
		return result, errors.New("incomplete instance cleanup readback")
	}
	live := map[string]types.Instance{}
	for _, reservation := range out.Reservations {
		if aws.ToString(reservation.OwnerId) != p.Network.AWS.AccountID {
			return result, errors.New("cleanup account mismatch")
		}
		for _, i := range reservation.Instances {
			id := aws.ToString(i.InstanceId)
			if _, ok := live[id]; ok {
				return result, errors.New("duplicate cleanup instance")
			}
			live[id] = i
		}
	}
	if len(live) != len(ids) {
		return result, errors.New("all original instance tombstones must remain visible for cleanup")
	}
	for _, t := range p.Targets {
		i, ok := live[r.Nodes[t.Name].InstanceID]
		if !ok || i.State == nil || i.State.Name != types.InstanceStateNameTerminated || aws.ToString(i.ClientToken) != p.Token(t) {
			return result, fmt.Errorf("%s is not the terminated original instance", t.Name)
		}
		tags := map[string]string{}
		for _, tag := range i.Tags {
			key := aws.ToString(tag.Key)
			if _, dup := tags[key]; dup {
				return result, errors.New("duplicate instance ownership tag")
			}
			tags[key] = aws.ToString(tag.Value)
		}
		for k, v := range p.Tags(t) {
			if tags[k] != v {
				return result, fmt.Errorf("%s cleanup ownership mismatch", t.Name)
			}
		}
	}
	// Check every EIP's full identity/ownership and detachment before releasing any.
	// Erase only historical association checkpoints in a validation copy: termination
	// legitimately detaches them; the durable evidence remains untouched.
	validation := r
	validation.Nodes = map[string]NodeProgress{}
	for name, n := range r.Nodes {
		if n.Address != nil {
			a := *n.Address
			n.Address = &a
			n.Address.AssociationID = ""
			if a.Phase == "associated" {
				n.Address.Phase = "allocated"
			}
			if a.Phase == "releasing" || a.Phase == "released" {
				n.Address.AllocationID = ""
			}
		}
		validation.Nodes[name] = n
	}
	addresses, err := discoverAddresses(ctx, p, c, validation, map[string]types.Instance{})
	if err != nil {
		return result, err
	}
	for _, t := range p.Targets {
		n := r.Nodes[t.Name]
		a, found := addresses[t.Name]
		if !found && n.Address != nil && n.Address.Phase != "releasing" && n.Address.Phase != "released" {
			return result, fmt.Errorf("unresolved address on %s; no release inferred", t.Name)
		}
		if !found && n.Address != nil {
			if err = verifyAddressReleased(ctx, c, n.Address.AllocationID); err != nil {
				return result, fmt.Errorf("%s: %w", t.Name, err)
			}
		}
		if found && (n.Address.AllocationID != "" && (n.Address.AllocationID != aws.ToString(a.AllocationId) || n.Address.PublicIP != aws.ToString(a.PublicIp))) {
			return result, fmt.Errorf("cleanup address identity drift on %s", t.Name)
		}
		if found && n.Address.Phase == "released" {
			return result, fmt.Errorf("released address reappeared on %s", t.Name)
		}
	}
	for _, t := range p.Targets {
		if err = ctx.Err(); err != nil {
			return
		}
		n := r.Nodes[t.Name]
		a, found := addresses[t.Name]
		if n.Address == nil {
			continue
		}
		if found {
			if n.Address.Phase == "allocating" {
				n.Address.AllocationID, n.Address.PublicIP = aws.ToString(a.AllocationId), aws.ToString(a.PublicIp)
			}
			n.Address.Phase = "releasing"
			r.Nodes[t.Name] = n
			if err = save(); err != nil {
				return
			}
			_, err = c.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: a.AllocationId, NetworkBorderGroup: aws.String(p.Network.AWS.Region)}, func(o *ec2.Options) { o.RetryMaxAttempts = 1 })
			if err != nil {
				return result, fmt.Errorf("release %s; resume reconciles address absence: %w", t.Name, err)
			}
			if err = verifyAddressReleased(ctx, c, n.Address.AllocationID); err != nil {
				return
			}
		}
		n.Address.Phase = "released"
		r.Nodes[t.Name] = n
		if err = save(); err != nil {
			return
		}
	}
	r.Phase = "addresses-released"
	r.LastError = ""
	err = save()
	return
}
