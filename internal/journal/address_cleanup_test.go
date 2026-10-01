package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

// cleanupCloud starts with a synthetic, already-terminated fleet. It cannot
// allocate, associate, terminate, or reach any real endpoint.
type cleanupCloud struct {
	*testutil.Cloud
	addresses []ec2types.Address
	releases  int
	loseReply bool
}

func (c *cleanupCloud) DescribeIpamPools(context.Context, *ec2.DescribeIpamPoolsInput, ...func(*ec2.Options)) (*ec2.DescribeIpamPoolsOutput, error) {
	return &ec2.DescribeIpamPoolsOutput{IpamPools: []ec2types.IpamPool{{IpamPoolId: aws.String(c.Network.AWS.Provision.IPAMPoolID), OwnerId: aws.String(c.Network.AWS.AccountID), Locale: aws.String(c.Network.AWS.Region), State: "create-complete", AddressFamily: "ipv4", IpamScopeType: "public", PublicIpSource: "byoip", AwsService: "ec2"}}}, nil
}
func (c *cleanupCloud) DescribeAddresses(_ context.Context, in *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	if len(in.AllocationIds) == 1 {
		for _, a := range c.addresses {
			if aws.ToString(a.AllocationId) == in.AllocationIds[0] {
				return &ec2.DescribeAddressesOutput{Addresses: []ec2types.Address{a}}, nil
			}
		}
		return nil, &smithy.GenericAPIError{Code: "InvalidAllocationID.NotFound"}
	}
	return &ec2.DescribeAddressesOutput{Addresses: append([]ec2types.Address(nil), c.addresses...)}, nil
}
func (c *cleanupCloud) AllocateAddress(context.Context, *ec2.AllocateAddressInput, ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error) {
	return nil, errors.New("cleanup must never allocate")
}
func (c *cleanupCloud) AssociateAddress(context.Context, *ec2.AssociateAddressInput, ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error) {
	return nil, errors.New("cleanup must never associate")
}
func (c *cleanupCloud) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, opts ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	o := ec2.Options{}
	for _, f := range opts {
		f(&o)
	}
	if o.RetryMaxAttempts != 1 || aws.ToString(in.NetworkBorderGroup) != c.Network.AWS.Region {
		return nil, errors.New("unsafe cleanup request")
	}
	for j, a := range c.addresses {
		if aws.ToString(a.AllocationId) == aws.ToString(in.AllocationId) {
			if aws.ToString(a.AssociationId) != "" || aws.ToString(a.InstanceId) != "" || aws.ToString(a.NetworkInterfaceId) != "" {
				return nil, errors.New("still attached")
			}
			c.addresses = append(c.addresses[:j], c.addresses[j+1:]...)
			c.releases++
			if c.loseReply {
				return nil, errors.New("lost release acknowledgement")
			}
			return &ec2.ReleaseAddressOutput{}, nil
		}
	}
	return nil, errors.New("duplicate release")
}

func cleanupFixture(t *testing.T) (Dynamo, *database, provision.Plan, *cleanupCloud) {
	t.Helper()
	d, db, _ := setup(t)
	n := testutil.ProvisionNetwork(t)
	n.Nodes[0].Count = 13
	n.Nodes = append(n.Nodes, spec.NodeGroup{Name: "wallet", Role: "wallet", Count: 1, Architecture: "arm64", InstanceType: "t4g.large"})
	n.AWS.Provision.PublicIPv4 = true
	n.AWS.Provision.IPAMPoolID = "ipam-pool-00000000000000001"
	c := &cleanupCloud{Cloud: &testutil.Cloud{Network: n}}
	p, err := provision.Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, c)
	if err != nil {
		t.Fatal(err)
	}
	r := provision.NewRecord(p)
	r.Phase, r.Revision = "compute-ready", 420
	r.Bootstrap = &provision.BootstrapProgress{PlanID: p.ID, Phase: "hosts-ready", Nodes: map[string]provision.BootstrapNode{}}
	r.Deployment = &provision.DeploymentProgress{PlanID: p.ID, Phase: "interrupted", Stage: "render", Nodes: map[string]provision.DeploymentNode{}}
	r.Runtime = &provision.RuntimeState{DeploymentID: p.ID, UpgradeID: strings.Repeat("a", 64), Images: provision.FleetImages{}}
	r.Upgrade = &provision.UpgradeProgress{PlanID: r.Runtime.UpgradeID, Phase: "interrupted", From: provision.FleetImages{}, To: provision.FleetImages{}, Baseline: map[string]provision.Preservation{}, Completed: map[string]bool{}}
	pin := "example.com/dash/component@sha256:" + strings.Repeat("b", 64)
	for j, target := range p.Targets {
		id := fmt.Sprintf("i-%017x", j+1)
		allocation, ip := fmt.Sprintf("eipalloc-%017x", j+1), fmt.Sprintf("198.51.100.%d", j+1)
		r.Nodes[target.Name] = provision.NodeProgress{Phase: "present", AttemptedAt: r.CreatedAt, ObservedAt: r.CreatedAt, EC2State: "running", InstanceID: id, Address: &provision.AddressProgress{Phase: "associated", AttemptedAt: r.CreatedAt, AllocationID: allocation, PublicIP: ip, AssociationID: fmt.Sprintf("eipassoc-%017x", j+1)}}
		r.Bootstrap.Nodes[target.Name] = provision.BootstrapNode{Phase: "ready", ObservedAt: r.CreatedAt, DockerVersion: "29.0.0", ComposeVersion: "2.40.0"}
		r.Deployment.Nodes[target.Name] = provision.DeploymentNode{Phase: "pending"}
		images := provision.ImageSet{}
		for _, component := range spec.RoleComponents(target.Role) {
			images[component] = pin
		}
		r.Runtime.Images[target.Name] = images
		r.Upgrade.From[target.Name], r.Upgrade.To[target.Name] = images, images
		r.Upgrade.Baseline[target.Name] = provision.Preservation{CoreID: strings.Repeat("c", 64), CoreConfig: strings.Repeat("d", 64), CoreGenesis: strings.Repeat("e", 64), CoreStarted: r.CreatedAt.Format(time.RFC3339Nano), Containers: map[string]string{"drive": strings.Repeat("f", 64), "tenderdash": strings.Repeat("f", 64), "dapi": strings.Repeat("f", 64), "gateway": strings.Repeat("f", 64)}}
		if target.Role == "validator" {
			r.Upgrade.Completed[target.Name] = false
		}
		tags := []ec2types.Tag{}
		for k, v := range p.Tags(target) {
			tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		c.Instances = append(c.Instances, ec2types.Instance{InstanceId: aws.String(id), ClientToken: aws.String(p.Token(target)), Tags: tags, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated}})
		c.addresses = append(c.addresses, ec2types.Address{AllocationId: aws.String(allocation), PublicIp: aws.String(ip), PublicIpv4Pool: aws.String(n.AWS.Provision.IPAMPoolID), NetworkBorderGroup: aws.String(n.AWS.Region), Domain: "vpc", Tags: tags})
	}
	if err := r.Validate(p); err != nil {
		t.Fatal("current fixture must be valid before removing wallet helper", err)
	}
	// Historical wallet runtime/from/to each have Core only, exactly the Bonsia
	// failure. Do not fabricate a helper pin to make today's validator pass.
	delete(r.Runtime.Images["wallet-001"], "helper")
	if err := r.Validate(p); err == nil || !strings.Contains(err.Error(), "incomplete runtime images for wallet-001") {
		t.Fatal("fixture did not reproduce the historical wallet failure", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Explicit null, zero/empty fields, formatting and escapes must survive even
	// though normal typed remarshal would omit/compact them.
	data = bytes.Replace(data, []byte(`"planId": "`+r.Upgrade.PlanID+`",`), []byte(`"planId": "`+r.Upgrade.PlanID+`", "scope": "",`), 1)
	data = append(data[:len(data)-1], []byte(",\n  \"join\": null\n}")...)
	db.item = key(p)
	db.item["PlanID"], db.item["Revision"], db.item["Data"] = str(p.ID), number(r.Revision), str(string(data))
	return d, db, p, c
}

func rawHistory(t *testing.T, db *database) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value(db.item, "Data")), &fields); err != nil {
		t.Fatal(err)
	}
	out := map[string]json.RawMessage{}
	for _, field := range historyFields {
		if raw, ok := fields[field]; ok {
			out[field] = raw
		}
	}
	return out
}

func assertHistory(t *testing.T, db *database, before map[string]json.RawMessage) {
	t.Helper()
	after := rawHistory(t, db)
	for _, field := range historyFields {
		if !bytes.Equal(before[field], after[field]) {
			t.Fatalf("cleanup changed raw %s history", field)
		}
	}
}

func TestAddressCleanupLegacyWalletReleaseAndReplay(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			d, db, p, c := cleanupFixture(t)
			ctx := context.Background()
			before := rawHistory(t, db)
			if _, _, err := d.Read(ctx, p); err == nil {
				t.Fatal("normal read relaxed application validation")
			}
			if _, err := d.Acquire(ctx, p, "deploy"); err == nil || value(db.item, "Owner") != "" {
				t.Fatal("normal deploy/upgrade acquisition accepted legacy runtime")
			}
			identity := testutil.Identity{Account: p.Network.AWS.AccountID}
			if lost {
				c.loseReply = true
				r, err := provision.ReleaseAddresses(ctx, p, identity, c, d.AddressCleanup(), "lost")
				if err == nil || r.Phase != "interrupted" || c.releases != 1 || value(db.item, "Owner") != "" {
					t.Fatal("lost release did not retain resumable checkpoint", err)
				}
				assertHistory(t, db, before)
				c.loseReply = false
			}
			r, err := provision.ReleaseAddresses(ctx, p, identity, c, d.AddressCleanup(), "cleanup")
			if err != nil || r.Phase != "addresses-released" || c.releases != 14 || len(c.addresses) != 0 || value(db.item, "Owner") != "" {
				t.Fatal("legacy wallet cleanup failed", err)
			}
			assertHistory(t, db, before)
			if _, err := provision.ReleaseAddresses(ctx, p, identity, c, d.AddressCleanup(), "replay"); err != nil || c.releases != 14 {
				t.Fatal("retired replay repeated release", err)
			}
			assertHistory(t, db, before)
		})
	}
}

func TestAddressCleanupClaimsAndStaleCheckpoints(t *testing.T) {
	d, db, p, _ := cleanupFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"one", "two"} {
		wg.Go(func() { _, err := d.AddressCleanup().Acquire(ctx, p, owner); results <- err })
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("cleanup claim not exclusive")
	}
	if err := d.Release(ctx, p, value(db.item, "Owner")); err != nil {
		t.Fatal(err)
	}
	s := d.AddressCleanup()
	r, err := s.Acquire(ctx, p, "runner")
	if err != nil {
		t.Fatal(err)
	}
	r.Revision++
	if err = s.Save(ctx, r, "wrong"); err == nil {
		t.Fatal("wrong runner saved")
	}
	if err = s.Save(ctx, r, "runner"); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, r, "runner"); err != nil {
		t.Fatal("identical checkpoint retry rejected", err)
	}
	old := r
	r.Revision++
	r.LastError = "new diagnostic"
	if err = s.Save(ctx, r, "runner"); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, old, "runner"); err == nil {
		t.Fatal("stale same-owner save accepted")
	}
	if err = d.Save(ctx, r, "runner"); err == nil {
		t.Fatal("normal save relaxed upgrade validation")
	}
	if err = s.Release(ctx, p, "wrong"); err == nil {
		t.Fatal("wrong runner released")
	}
	if err = s.Release(ctx, p, "runner"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.AddressCleanup().Acquire(ctx, p, "successor"); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, r, "runner"); err == nil {
		t.Fatal("stale owner saved")
	}
}

func TestAddressCleanupRefusesHistoryChanges(t *testing.T) {
	for _, field := range historyFields {
		t.Run(field, func(t *testing.T) {
			d, db, p, _ := cleanupFixture(t)
			before := rawHistory(t, db)
			s := d.AddressCleanup()
			r, err := s.Acquire(context.Background(), p, "runner")
			if err != nil {
				t.Fatal(err)
			}
			r.Revision++
			switch field {
			case "bootstrap":
				r.Bootstrap = nil
			case "deployment":
				r.Deployment.Stage = "stopped"
			case "join":
				r.Join = &provision.JoinProgress{Phase: "joined"}
			case "runtime":
				r.Runtime.Images["wallet-001"]["helper"] = "invented"
			case "upgrade":
				r.Upgrade = nil
			}
			if err = s.Save(context.Background(), r, "runner"); err == nil || db.saves != 0 {
				t.Fatal("cleanup rewrote application history")
			}
			assertHistory(t, db, before)
		})
	}
}

func TestAddressCleanupRejectsMalformedAllocationJournal(t *testing.T) {
	for _, fault := range []string{"network-key", "plan-id", "revision-attribute", "unknown-field", "trailing-json", "oversized", "plan-account", "plan-region", "plan-target", "schema", "kind", "revision", "created-at", "updated-at", "target-set", "target-name", "phase", "instance-phase", "instance-attempt", "instance-observation", "duplicate-instance", "address-phase", "address-attempt", "allocation-id", "private-ip", "association-id", "duplicate-allocation", "retired-unfinished"} {
		t.Run(fault, func(t *testing.T) {
			d, db, p, c := cleanupFixture(t)
			var r provision.Record
			if err := json.Unmarshal([]byte(value(db.item, "Data")), &r); err != nil {
				t.Fatal(err)
			}
			first, second := p.Targets[0].Name, p.Targets[1].Name
			n := r.Nodes[first]
			switch fault {
			case "plan-account":
				r.Plan.Network.AWS.AccountID = "000000000000"
			case "plan-region":
				r.Plan.Network.AWS.Region = "us-east-1"
			case "plan-target":
				r.Plan.Targets[0].Role = "wallet"
			case "schema":
				r.APIVersion = "wrong"
			case "kind":
				r.Kind = "wrong"
			case "revision":
				r.Revision = -1
			case "created-at":
				r.CreatedAt = time.Time{}
			case "updated-at":
				r.UpdatedAt = time.Time{}
			case "target-set":
				delete(r.Nodes, second)
			case "target-name":
				r.Nodes["foreign"] = r.Nodes[second]
				delete(r.Nodes, second)
			case "phase":
				r.Phase = "complete"
			case "instance-phase":
				n.Phase = "missing"
			case "instance-attempt":
				n.AttemptedAt = time.Time{}
			case "instance-observation":
				n.ObservedAt = time.Time{}
			case "duplicate-instance":
				n.InstanceID = r.Nodes[second].InstanceID
			case "address-phase":
				n.Address.Phase = "unknown"
			case "address-attempt":
				n.Address.AttemptedAt = time.Time{}
			case "allocation-id":
				n.Address.AllocationID = ""
			case "private-ip":
				n.Address.PublicIP = "10.0.0.1"
			case "association-id":
				n.Address.AssociationID = ""
			case "duplicate-allocation":
				n.Address.AllocationID = r.Nodes[second].Address.AllocationID
			case "retired-unfinished":
				r.Phase = "addresses-released"
			}
			r.Nodes[first] = n
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			db.item["Data"], db.item["Revision"] = str(string(data)), number(r.Revision)
			switch fault {
			case "network-key":
				db.item["Network"] = str("other-network")
			case "plan-id":
				db.item["PlanID"] = str(strings.Repeat("f", 64))
			case "revision-attribute":
				db.item["Revision"] = number(r.Revision + 1)
			case "unknown-field":
				db.item["Data"] = str(strings.TrimSuffix(string(data), "}") + `,"unexpected":true}`)
			case "trailing-json":
				db.item["Data"] = str(string(data) + ` {}`)
			case "oversized":
				db.item["Data"] = str(strings.Repeat(" ", 300001))
			}
			if _, err := provision.ReleaseAddresses(context.Background(), p, testutil.Identity{Account: p.Network.AWS.AccountID}, c, d.AddressCleanup(), "cleanup"); err == nil || c.releases != 0 || value(db.item, "Owner") != "" || db.saves != 0 {
				t.Fatal("malformed journal accepted or claimed/mutated", err)
			}
		})
	}
}

func TestAddressCleanupLegacyWalletStillRequiresLiveOwnershipAndDetachment(t *testing.T) {
	for _, fault := range []string{"caller-account", "running", "stopped", "instance-token", "instance-tag", "address-tag", "address-pool", "address-region", "address-identity", "association", "eni", "attached-instance", "missing-address"} {
		t.Run(fault, func(t *testing.T) {
			d, db, p, c := cleanupFixture(t)
			before := rawHistory(t, db)
			identity := testutil.Identity{Account: p.Network.AWS.AccountID}
			switch fault {
			case "caller-account":
				identity.Account = "000000000000"
			case "running":
				c.Instances[13].State.Name = ec2types.InstanceStateNameRunning
			case "stopped":
				c.Instances[13].State.Name = ec2types.InstanceStateNameStopped
			case "instance-token":
				c.Instances[13].ClientToken = aws.String("foreign")
			case "instance-tag":
				c.Instances[13].Tags = nil
			case "address-tag":
				c.addresses[13].Tags = nil
			case "address-pool":
				c.addresses[13].PublicIpv4Pool = aws.String("foreign")
			case "address-region":
				c.addresses[13].NetworkBorderGroup = aws.String("us-east-1")
			case "address-identity":
				c.addresses[13].AllocationId = aws.String("eipalloc-fffffffffffffffff")
			case "association":
				c.addresses[13].AssociationId = aws.String("attached")
			case "eni":
				c.addresses[13].NetworkInterfaceId = aws.String("eni-attached")
			case "attached-instance":
				c.addresses[13].InstanceId = c.Instances[13].InstanceId
			case "missing-address":
				c.addresses = c.addresses[:13]
			}
			if _, err := provision.ReleaseAddresses(context.Background(), p, identity, c, d.AddressCleanup(), "cleanup"); err == nil || c.releases != 0 {
				t.Fatal("legacy metadata bypassed live cleanup checks", err)
			}
			assertHistory(t, db, before)
		})
	}
}

func TestAddressCleanupDoesNotCreateMissingJournal(t *testing.T) {
	d, db, p := setup(t)
	if _, err := d.AddressCleanup().Acquire(context.Background(), p, "runner"); !errors.Is(err, ErrNotFound) || db.item != nil {
		t.Fatal("cleanup created a new journal", err)
	}
}

func TestAddressCleanupLegacyWalletWithPurgedTombstones(t *testing.T) {
	for _, response := range []string{"empty", "not-found", "denied", "other-instance"} {
		t.Run(response, func(t *testing.T) {
			d, db, p, c := cleanupFixture(t)
			before := rawHistory(t, db)
			purged := aws.ToString(c.Instances[13].InstanceId)
			c.Instances = c.Instances[:13]
			c.Describe = func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
				if len(in.InstanceIds) == 1 && in.InstanceIds[0] == purged {
					switch response {
					case "not-found":
						return nil, &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound"}
					case "denied":
						return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation"}
					case "other-instance":
						return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{OwnerId: aws.String(p.Network.AWS.AccountID), Instances: c.Instances[:1]}}}, nil
					default:
						return &ec2.DescribeInstancesOutput{}, nil
					}
				}
				return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{OwnerId: aws.String(p.Network.AWS.AccountID), Instances: c.Instances}}}, nil
			}
			r, err := provision.ReleaseAddresses(context.Background(), p, testutil.Identity{Account: p.Network.AWS.AccountID}, c, d.AddressCleanup(), "cleanup")
			if response == "empty" || response == "not-found" {
				if err != nil || r.Phase != "addresses-released" || c.releases != 14 {
					t.Fatal("legacy wallet/purged instance cleanup failed", err)
				}
			} else if err == nil || c.releases != 0 {
				t.Fatal("unproven purged instance cleanup accepted", err)
			}
			assertHistory(t, db, before)
		})
	}
}

func TestAddressCleanupAmbiguousOrCorruptClaimRemainsFenced(t *testing.T) {
	for _, fault := range []string{"lost-response", "nil-response", "wrong-owner", "wrong-plan", "bad-revision", "bad-node"} {
		t.Run(fault, func(t *testing.T) {
			d, db, p, c := cleanupFixture(t)
			db.claimResponse = func(out *dynamodb.UpdateItemOutput) (*dynamodb.UpdateItemOutput, error) {
				switch fault {
				case "lost-response":
					return nil, errors.New("claim response lost after commit")
				case "nil-response":
					return nil, nil
				case "wrong-owner":
					out.Attributes["Owner"] = str("another-runner")
				case "wrong-plan":
					out.Attributes["PlanID"] = str("another-plan")
				case "bad-revision":
					out.Attributes["Revision"] = number(-1)
				case "bad-node":
					var r provision.Record
					_ = json.Unmarshal([]byte(value(out.Attributes, "Data")), &r)
					delete(r.Nodes, p.Targets[0].Name)
					data, _ := json.Marshal(r)
					out.Attributes["Data"] = str(string(data))
				}
				return out, nil
			}
			if _, err := provision.ReleaseAddresses(context.Background(), p, testutil.Identity{Account: p.Network.AWS.AccountID}, c, d.AddressCleanup(), "ambiguous"); err == nil || c.releases != 0 || value(db.item, "Owner") != "ambiguous" {
				t.Fatal("unverified claim allowed release or was blindly unlocked", err)
			}
			db.claimResponse = nil
			if _, err := d.AddressCleanup().Acquire(context.Background(), p, "successor"); err == nil {
				t.Fatal("ambiguous claim stolen")
			}
		})
	}
}

func TestAddressCleanupDoesNotRelaxNormalApplicationValidation(t *testing.T) {
	for _, field := range historyFields {
		t.Run(field, func(t *testing.T) {
			d, db, p, _ := cleanupFixture(t)
			var r provision.Record
			_ = json.Unmarshal([]byte(value(db.item, "Data")), &r)
			pin := r.Runtime.Images["wallet-001"]["core"]
			for _, images := range []provision.FleetImages{r.Runtime.Images, r.Upgrade.From, r.Upgrade.To} {
				images["wallet-001"]["helper"] = pin
			}
			if err := r.Validate(p); err != nil {
				t.Fatal("current application fixture invalid", err)
			}
			switch field {
			case "bootstrap":
				r.Bootstrap.Phase = "invalid"
			case "deployment":
				r.Deployment.Phase = "invalid"
			case "join":
				r.Join = &provision.JoinProgress{PlanID: p.ID, Phase: "joined"}
			case "runtime":
				r.Runtime.Images["wallet-001"]["core"] = "mutable:latest"
			case "upgrade":
				r.Upgrade.Phase = "invalid"
			}
			if err := r.ValidateAddressCleanup(p); err != nil {
				t.Fatal("application metadata became cleanup authority", err)
			}
			if err := r.Validate(p); err == nil {
				t.Fatal("normal application validator relaxed")
			}
			r.Revision++
			if err := d.Save(context.Background(), r, "runner"); err == nil || db.saves != 0 {
				t.Fatal("normal journal encoder relaxed")
			}
		})
	}
}
