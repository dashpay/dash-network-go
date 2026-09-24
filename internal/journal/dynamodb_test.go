package journal

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

type database struct {
	mu              sync.Mutex
	item            map[string]types.AttributeValue
	table           types.TableDescription
	failPut         error
	losePutResponse bool
	saves           int
	reads           int
}

func clone(in map[string]types.AttributeValue) map[string]types.AttributeValue {
	out := map[string]types.AttributeValue{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func (d *database) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{Table: &d.table}, nil
}
func (d *database) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	if !aws.ToBool(in.ConsistentRead) {
		return nil, errors.New("journal read must be strongly consistent")
	}
	return &dynamodb.GetItemOutput{Item: clone(d.item)}, nil
}
func (d *database) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failPut != nil {
		return nil, d.failPut
	}
	if aws.ToString(in.ConditionExpression) != "attribute_not_exists(#network)" || in.ExpressionAttributeNames["#network"] != "Network" {
		return nil, errors.New("missing create-if-absent condition")
	}
	if d.item != nil {
		return nil, &types.ConditionalCheckFailedException{}
	}
	d.item = clone(in.Item)
	if d.losePutResponse {
		return nil, errors.New("lost successful Put response")
	}
	return &dynamodb.PutItemOutput{}, nil
}
func (d *database) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if value(in.Key, "Network") != value(d.item, "Network") || value(in.ExpressionAttributeValues, ":plan") != value(d.item, "PlanID") || in.ExpressionAttributeNames["#plan"] != "PlanID" || in.ExpressionAttributeNames["#owner"] != "Owner" {
		return nil, &types.ConditionalCheckFailedException{}
	}
	owner := value(in.ExpressionAttributeValues, ":owner")
	switch aws.ToString(in.UpdateExpression) {
	case "SET #owner = :owner":
		if aws.ToString(in.ConditionExpression) != "#plan = :plan AND attribute_not_exists(#owner)" {
			return nil, errors.New("unconditional claim")
		}
		if value(d.item, "Owner") != "" {
			return nil, &types.ConditionalCheckFailedException{}
		}
		d.item["Owner"] = str(owner)
	case "SET #data = :data, #revision = :next", "REMOVE #owner":
		expected := "#plan = :plan AND #owner = :owner"
		if aws.ToString(in.UpdateExpression) != "REMOVE #owner" {
			expected += " AND #revision = :previous"
			actual, ok := d.item["Revision"].(*types.AttributeValueMemberN)
			previous, prevOK := in.ExpressionAttributeValues[":previous"].(*types.AttributeValueMemberN)
			if !ok || !prevOK || actual.Value != previous.Value {
				return nil, &types.ConditionalCheckFailedException{}
			}
		}
		if aws.ToString(in.ConditionExpression) != expected {
			return nil, errors.New("unconditional save/release")
		}
		if owner != value(d.item, "Owner") {
			return nil, &types.ConditionalCheckFailedException{}
		}
		if aws.ToString(in.UpdateExpression) == "REMOVE #owner" {
			delete(d.item, "Owner")
		} else {
			d.item["Data"] = in.ExpressionAttributeValues[":data"]
			d.item["Revision"] = in.ExpressionAttributeValues[":next"]
			d.saves++
		}
	default:
		return nil, errors.New("unexpected update")
	}
	return &dynamodb.UpdateItemOutput{Attributes: clone(d.item)}, nil
}
func setup(t *testing.T) (Dynamo, *database, provision.Plan) {
	t.Helper()
	n := testutil.ProvisionNetwork(t)
	p, err := provision.Prepare(context.Background(), n, testutil.Identity{Account: n.AWS.AccountID}, &testutil.Cloud{Network: n})
	if err != nil {
		t.Fatal(err)
	}
	d := &database{table: types.TableDescription{TableArn: aws.String("arn:aws:dynamodb:" + n.AWS.Region + ":" + n.AWS.AccountID + ":table/" + n.AWS.Provision.StateTable), TableStatus: types.TableStatusActive, KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("Network"), KeyType: types.KeyTypeHash}}, AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("Network"), AttributeType: types.ScalarAttributeTypeS}}}}
	return Dynamo{Client: d, Table: n.AWS.Provision.StateTable}, d, p
}
func TestConditionalClaimSaveRecoveryAndStaleOwner(t *testing.T) {
	d, _, p := setup(t)
	ctx := context.Background()
	if err := d.Verify(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Read(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing record mishandled")
	}
	r, err := d.Acquire(ctx, p, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Acquire(ctx, p, "second"); err == nil {
		t.Fatal("simultaneous claim succeeded")
	}
	r.Phase = "provisioning"
	r.Revision++
	if err = d.Save(ctx, r, "first"); err != nil {
		t.Fatal(err)
	}
	if err = d.Release(ctx, p, "wrong"); err == nil {
		t.Fatal("wrong owner released claim")
	}
	if err = d.Release(ctx, p, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Acquire(ctx, p, "second"); err != nil {
		t.Fatal(err)
	}
	if err = d.Save(ctx, r, "first"); err == nil {
		t.Fatal("stale owner wrote state")
	}
	if err = d.Release(ctx, p, "first"); err == nil {
		t.Fatal("stale owner released current claim")
	}
	current, owner, err := d.Read(ctx, p)
	if err != nil || owner != "second" || current.Phase != "provisioning" {
		t.Fatal("recovery lost record", err)
	}
}
func TestConcurrentAcquireHasExactlyOneWinner(t *testing.T) {
	d, _, p := setup(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"one", "two"} {
		wg.Go(func() { _, err := d.Acquire(ctx, p, owner); results <- err })
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
		t.Fatal("claim was not exclusive")
	}
}
func TestAmbiguousCreateRemainsClaimed(t *testing.T) {
	d, db, p := setup(t)
	db.losePutResponse = true
	ctx := context.Background()
	if _, err := d.Acquire(ctx, p, "lost-response"); err == nil {
		t.Fatal("expected ambiguous error")
	}
	if _, err := d.Acquire(ctx, p, "new-runner"); err == nil {
		t.Fatal("stole ambiguous claim")
	}
	_, owner, err := d.Read(ctx, p)
	if err != nil || owner != "lost-response" {
		t.Fatal("lost ownership evidence")
	}
}
func TestJournalRefusesWrongPlanOrCorruptPayload(t *testing.T) {
	for _, kind := range []string{"plan-id", "payload-plan", "unknown-field", "trailing-json", "huge"} {
		t.Run(kind, func(t *testing.T) {
			d, db, p := setup(t)
			ctx := context.Background()
			if _, err := d.Acquire(ctx, p, "one"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "plan-id":
				db.item["PlanID"] = str("another-plan")
			case "payload-plan":
				db.item["Data"] = str(`{"apiVersion":"invalid"}`)
			case "unknown-field":
				db.item["Data"] = str(`{"unexpected":"field"}`)
			case "trailing-json":
				db.item["Data"] = str(value(db.item, "Data") + ` {}`)
			case "huge":
				db.item["Data"] = str(string(make([]byte, 300001)))
			}
			if _, _, err := d.Read(ctx, p); err == nil {
				t.Fatal("invalid journal accepted")
			}
		})
	}
}
func TestJournalTableScopeAndSchema(t *testing.T) {
	for _, kind := range []string{"region", "account", "schema", "type", "inactive", "table-name"} {
		t.Run(kind, func(t *testing.T) {
			d, db, p := setup(t)
			switch kind {
			case "region":
				db.table.TableArn = aws.String("arn:aws:dynamodb:eu-west-1:123456789012:table/" + d.Table)
			case "account":
				db.table.TableArn = aws.String("arn:aws:dynamodb:us-west-2:000000000000:table/" + d.Table)
			case "schema":
				db.table.KeySchema = append(db.table.KeySchema, types.KeySchemaElement{AttributeName: aws.String("Other"), KeyType: types.KeyTypeRange})
			case "type":
				db.table.AttributeDefinitions[0].AttributeType = types.ScalarAttributeTypeN
			case "inactive":
				db.table.TableStatus = types.TableStatusCreating
			case "table-name":
				d.Table = "other"
			}
			if err := d.Verify(context.Background(), p); err == nil {
				t.Fatal("invalid table accepted")
			}
		})
	}
}

func TestDelayedSameOwnerCheckpointCannotRegressProgress(t *testing.T) {
	d, _, p := setup(t)
	ctx := context.Background()
	r, err := d.Acquire(ctx, p, "runner")
	if err != nil {
		t.Fatal(err)
	}
	r.Phase = "provisioning"
	r.Revision++
	if err = d.Save(ctx, r, "runner"); err != nil {
		t.Fatal(err)
	}
	old := r
	r.Phase = "interrupted"
	r.LastError = "preserve this newer diagnostic"
	r.Revision++
	if err = d.Save(ctx, r, "runner"); err != nil {
		t.Fatal(err)
	}
	if err = d.Save(ctx, old, "runner"); err == nil {
		t.Fatal("delayed checkpoint regressed journal")
	}
	current, _, err := d.Read(ctx, p)
	if err != nil || current.Revision != 2 || current.LastError != r.LastError {
		t.Fatal("newer progress was lost", err)
	}
}

func TestBootstrapCheckpointRoundTripAndPartialReadinessRefusal(t *testing.T) {
	d, _, p := setup(t)
	ctx := context.Background()
	r, err := d.Acquire(ctx, p, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	r.Bootstrap = &provision.BootstrapProgress{PlanID: p.ID, Phase: "interrupted", Nodes: map[string]provision.BootstrapNode{}}
	for _, target := range p.Targets {
		r.Bootstrap.Nodes[target.Name] = provision.BootstrapNode{Phase: "unknown"}
	}
	r.Revision++
	if err = d.Save(ctx, r, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	current, _, err := d.Read(ctx, p)
	if err != nil || current.Bootstrap.PlanID != p.ID || len(current.Bootstrap.Nodes) != len(p.Targets) {
		t.Fatal("bootstrap journal lost", err)
	}
	r.Bootstrap.Phase = "hosts-ready"
	r.Revision++
	if err = d.Save(ctx, r, "bootstrap"); err == nil {
		t.Fatal("partial readiness persisted as success")
	}
}
