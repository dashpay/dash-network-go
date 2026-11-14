// Package journal stores private operation state and non-expiring runner claims.
// The table is provisioned once out-of-band; this package never creates/deletes it.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/dashpay/dash-network-go/internal/provision"
)

type API interface {
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

type Dynamo struct {
	Client API
	Table  string
}

var ErrNotFound = errors.New("operation not found")

func (d Dynamo) Verify(ctx context.Context, p provision.Plan) error {
	if d.Table != p.Network.AWS.Provision.StateTable {
		return errors.New("journal table differs from provision plan")
	}
	out, err := d.Client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(d.Table)})
	if err != nil {
		return fmt.Errorf("describe journal table: %w", err)
	}
	if out == nil || out.Table == nil {
		return errors.New("missing journal table")
	}
	table := out.Table
	arn := strings.Split(aws.ToString(table.TableArn), ":")
	if len(arn) != 6 || arn[0] != "arn" || arn[2] != "dynamodb" || arn[3] != p.Network.AWS.Region || arn[4] != p.Network.AWS.AccountID || arn[5] != "table/"+d.Table || table.TableStatus != types.TableStatusActive || len(table.KeySchema) != 1 || aws.ToString(table.KeySchema[0].AttributeName) != "Network" || table.KeySchema[0].KeyType != types.KeyTypeHash {
		return errors.New("journal table must be ACTIVE in the configured account/region, with only the Network string partition key")
	}
	for _, a := range table.AttributeDefinitions {
		if aws.ToString(a.AttributeName) == "Network" && a.AttributeType == types.ScalarAttributeTypeS {
			return nil
		}
	}
	return errors.New("journal Network partition key must be a string")
}
func number(n int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}

func str(s string) types.AttributeValue { return &types.AttributeValueMemberS{Value: s} }
func value(item map[string]types.AttributeValue, k string) string {
	v, ok := item[k].(*types.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return v.Value
}
func key(p provision.Plan) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"Network": str(p.Key())}
}
func conditional(err error) bool {
	var target *types.ConditionalCheckFailedException
	return errors.As(err, &target)
}

func encode(r provision.Record) (string, error) {
	if err := r.Validate(r.Plan); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(b) > 300000 {
		return "", errors.New("operation journal exceeds conservative DynamoDB item budget")
	}
	return string(b), nil
}
func decode(item map[string]types.AttributeValue, p provision.Plan) (provision.Record, error) {
	return decodeRecord(item, p, provision.Record.Validate)
}

func decodeRecord(item map[string]types.AttributeValue, p provision.Plan, validate func(provision.Record, provision.Plan) error) (provision.Record, error) {
	if value(item, "Network") != p.Key() || value(item, "PlanID") != p.ID {
		return provision.Record{}, errors.New("network is already bound to a different plan; no automatic reset, replacement, or state deletion (a destroyed network's name is reused by a new provision-plan once its addresses are released)")
	}
	r, err := decodePayload(item)
	if err != nil {
		return r, err
	}
	return r, validate(r, p)
}

func decodePayload(item map[string]types.AttributeValue) (provision.Record, error) {
	data := value(item, "Data")
	if data == "" || len(data) > 300000 {
		return provision.Record{}, errors.New("invalid journal payload size")
	}
	var r provision.Record
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return r, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return r, errors.New("trailing journal payload")
	}
	revision, ok := item["Revision"].(*types.AttributeValueMemberN)
	if !ok || revision.Value != strconv.FormatInt(r.Revision, 10) {
		return r, errors.New("journal revision mismatch")
	}
	return r, nil
}

// retired decodes the live item under a network key on its own terms, whatever
// plan it belongs to, and returns it only if it proves that network is gone:
// an unclaimed operation, consistent with its own plan, whose address
// retirement finished. An item this package did not write is never retired.
func retired(item map[string]types.AttributeValue, network string) (provision.Record, error) {
	if value(item, "Owner") != "" {
		return provision.Record{}, errors.New("the operation has an active runner claim")
	}
	if len(item) != 4 {
		return provision.Record{}, errors.New("the journal item is not a provision operation")
	}
	r, err := decodePayload(item)
	if err != nil {
		return r, err
	}
	if value(item, "Network") != network || r.Plan.Key() != network || value(item, "PlanID") != r.Plan.ID {
		return r, errors.New("the journal item does not belong to this network key")
	}
	return r, r.Retired()
}

// ErrNotRetired explains why the operation under a network key cannot be
// succeeded. It is advice for whoever is planning, never an authorization.
var ErrNotRetired = errors.New("the existing operation is not retired")

// Succession tells provision-plan which plan p's network must be planned as,
// given what the journal holds under its key. p is the plan of the network
// definition alone. The answer is the ID the plan must succeed:
//
//   - "" when the key is free, or holds p's own operation;
//   - the retired plan's ID, with reused true, when the key holds a retired
//     operation: the name is free again for the plan that succeeds it;
//   - the ID the live operation's plan succeeds, when that plan is this same
//     network: planning it again yields the same plan, so it can be resumed.
//
// Anything else is ErrNotRetired. Read-only, and never an authorization:
// provision reads the journal again and Supersede decides.
func (d Dynamo) Succession(ctx context.Context, p provision.Plan) (succeeds string, reused bool, err error) {
	if p.Succeeds != "" {
		return "", false, errors.New("succession starts from the plan of the network definition")
	}
	item, err := d.readItem(ctx, p)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	r, notRetired := retired(item, p.Key())
	if notRetired == nil {
		return r.Plan.ID, true, nil
	}
	if live, decodeErr := decodePayload(item); decodeErr == nil && value(item, "PlanID") == live.Plan.ID {
		same := p
		if live.Plan.Succeeds != "" {
			same, err = p.Successor(live.Plan.Succeeds)
		}
		if err == nil && same.ID == live.Plan.ID {
			return live.Plan.Succeeds, false, nil
		}
	}
	return "", false, fmt.Errorf("%w: %v", ErrNotRetired, notRetired)
}

// Supersede is Acquire for a plan that succeeds a retired one. The key is
// handed over only if the live item is exactly the plan p names, unclaimed and
// retired. Nothing is deleted: the retired item is first copied, verbatim, to
// the archive key of the revision that was read (create-if-absent, so a repeat
// is harmless), then replaced by p's fresh operation in one write conditional
// on the plan, that same revision and the absence of a claim. Of two runners,
// one write wins and the other finds an operation that is no longer the one
// it read.
//
// The two writes are not atomic. If the handover stops between them, the
// retired operation simply has a copy, and the next attempt goes on. If the
// retired operation has changed by then (an older release could still write
// to one), that attempt archives the new revision under its own key: a copy
// of an earlier revision never stands in its way.
func (d Dynamo) Supersede(ctx context.Context, p provision.Plan, owner string) (provision.Record, error) {
	if owner == "" {
		return provision.Record{}, errors.New("empty runner ID")
	}
	if p.Succeeds == "" {
		return provision.Record{}, errors.New("plan does not succeed a retired operation")
	}
	item, err := d.readItem(ctx, p)
	if errors.Is(err, ErrNotFound) {
		return provision.Record{}, errors.New("plan succeeds an operation this journal does not hold; regenerate the plan")
	}
	if err != nil {
		return provision.Record{}, err
	}
	if value(item, "PlanID") == p.ID {
		// Already handed over to this plan, by an earlier run: resume it.
		return d.Acquire(ctx, p, owner)
	}
	if value(item, "PlanID") != p.Succeeds {
		return provision.Record{}, errors.New("network is bound to a plan other than the one this plan succeeds; regenerate the plan")
	}
	old, err := retired(item, p.Key())
	if err != nil {
		return provision.Record{}, fmt.Errorf("%w: %v", ErrNotRetired, err)
	}
	r, err := provision.NewSuccessor(p, old)
	if err != nil {
		return provision.Record{}, err
	}
	data, err := encode(r)
	if err != nil {
		return provision.Record{}, err
	}
	archive := map[string]types.AttributeValue{"Network": str(old.Plan.ArchiveKey(old.Revision)), "PlanID": item["PlanID"], "Data": item["Data"], "Revision": item["Revision"]}
	_, err = d.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(d.Table), Item: archive, ConditionExpression: aws.String("attribute_not_exists(#network)"), ExpressionAttributeNames: map[string]string{"#network": "Network"}})
	if conditional(err) {
		// An earlier attempt archived this revision. It must be this very item.
		var kept *dynamodb.GetItemOutput
		kept, err = d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: map[string]types.AttributeValue{"Network": archive["Network"]}, ConsistentRead: aws.Bool(true)})
		if err == nil && (kept == nil || len(kept.Item) != len(archive) || value(kept.Item, "PlanID") != value(archive, "PlanID") || value(kept.Item, "Data") != value(archive, "Data") || !sameNumber(kept.Item["Revision"], archive["Revision"])) {
			err = errors.New("the archive key of this revision holds different data; nothing replaced")
		}
	}
	if err != nil {
		return provision.Record{}, fmt.Errorf("archive retired operation: %w", err)
	}
	next := key(p)
	next["PlanID"] = str(p.ID)
	next["Data"] = str(data)
	next["Owner"] = str(owner)
	next["Revision"] = number(0)
	_, err = d.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.Table), Item: next, ConditionExpression: aws.String("#plan = :retired AND #revision = :revision AND attribute_not_exists(#owner)"),
		ExpressionAttributeNames: map[string]string{"#plan": "PlanID", "#revision": "Revision", "#owner": "Owner"}, ExpressionAttributeValues: map[string]types.AttributeValue{":retired": str(p.Succeeds), ":revision": number(old.Revision)},
	})
	if err == nil {
		return r, nil
	}
	if !conditional(err) {
		// The write may have been accepted: like a lost Acquire response, the
		// claim may exist. Inspect the operation; never assume it failed.
		return provision.Record{}, fmt.Errorf("replace retired operation (outcome may be uncertain; inspect operation): %w", err)
	}
	// The item is no longer the one that was read. If it is this very write,
	// accepted before a retry of it was refused, the handover is done.
	// Otherwise another runner won, and the ordinary claim rules decide.
	item, err = d.readItem(ctx, p)
	if err != nil {
		return provision.Record{}, err
	}
	if value(item, "PlanID") == p.ID && value(item, "Owner") == owner && value(item, "Data") == data && sameNumber(item["Revision"], number(0)) {
		return r, nil
	}
	return d.Acquire(ctx, p, owner)
}

// Archived reads the operation of a plan whose network key has been reused. It
// is found from the live operation, each operation naming the archive key of
// the one it replaced.
func (d Dynamo) Archived(ctx context.Context, p provision.Plan) (provision.Record, error) {
	item, err := d.readItem(ctx, p)
	if err != nil {
		return provision.Record{}, err
	}
	for range 100 {
		newer, err := decodePayload(item)
		if err != nil {
			return provision.Record{}, err
		}
		if newer.Predecessor == nil {
			return provision.Record{}, ErrNotFound
		}
		out, err := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: map[string]types.AttributeValue{"Network": str(newer.Predecessor.Archive)}, ConsistentRead: aws.Bool(true)})
		if err != nil {
			return provision.Record{}, err
		}
		if out == nil || len(out.Item) != 4 || value(out.Item, "Network") != newer.Predecessor.Archive || value(out.Item, "PlanID") != newer.Predecessor.PlanID {
			return provision.Record{}, errors.New("missing or invalid archived operation")
		}
		item = out.Item
		if newer.Predecessor.PlanID != p.ID {
			continue
		}
		r, err := decodePayload(item)
		if err != nil {
			return r, err
		}
		if r.Revision != newer.Predecessor.Revision {
			return r, errors.New("archived operation is not the revision that was replaced")
		}
		if err = r.ValidateAddressCleanup(p); err != nil {
			return r, err
		}
		return r, r.Retired()
	}
	return provision.Record{}, ErrNotFound
}

func sameNumber(a, b types.AttributeValue) bool {
	x, ok := a.(*types.AttributeValueMemberN)
	y, also := b.(*types.AttributeValueMemberN)
	return ok && also && x.Value == y.Value
}

// Read is strongly consistent so a resume never relies on an eventually
// consistent lock read. Conditional writes remain the actual exclusion gate.
func (d Dynamo) Read(ctx context.Context, p provision.Plan) (provision.Record, string, error) {
	item, err := d.readItem(ctx, p)
	if err != nil {
		return provision.Record{}, "", err
	}
	r, err := decode(item, p)
	return r, value(item, "Owner"), err
}

func (d Dynamo) readItem(ctx context.Context, p provision.Plan) (map[string]types.AttributeValue, error) {
	out, err := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: key(p), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, err
	}
	if out == nil || len(out.Item) == 0 {
		return nil, ErrNotFound
	}
	return out.Item, nil
}
func (d Dynamo) Acquire(ctx context.Context, p provision.Plan, owner string) (provision.Record, error) {
	if owner == "" {
		return provision.Record{}, errors.New("empty runner ID")
	}
	if p.Succeeds != "" {
		// A successor's operation is created only by Supersede, from provision.
		// Every other stage may claim it once it exists, and nothing else.
		return d.claimLive(ctx, p, owner)
	}
	r := provision.NewRecord(p)
	data, err := encode(r)
	if err != nil {
		return r, err
	}
	item := key(p)
	item["PlanID"] = str(p.ID)
	item["Data"] = str(data)
	item["Owner"] = str(owner)
	item["Revision"] = number(0)
	_, err = d.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(d.Table), Item: item, ConditionExpression: aws.String("attribute_not_exists(#network)"), ExpressionAttributeNames: map[string]string{"#network": "Network"}})
	if err == nil {
		return r, nil
	}
	if !conditional(err) {
		return r, err
	}
	// Do not overwrite an existing record. It may be an interrupted operation.
	return d.claimLive(ctx, p, owner)
}

// claimLive claims an existing operation for a stage that will work on it. A
// retired operation is never claimed here: it would let that stage write new
// checkpoints into the proof that its network is gone. Only address cleanup,
// through its own store, still reads one, and it changes nothing.
func (d Dynamo) claimLive(ctx context.Context, p provision.Plan, owner string) (provision.Record, error) {
	existing, _, err := d.Read(ctx, p)
	if err != nil {
		return provision.Record{}, err
	}
	if existing.Phase == "addresses-released" {
		return provision.Record{}, provision.ErrRetired
	}
	claimed, err := d.claim(ctx, p, owner)
	if err != nil {
		return provision.Record{}, err
	}
	r, err := decode(claimed, p)
	if err == nil && r.Phase == "addresses-released" {
		// Retired between the read and the claim: give the claim back unused.
		if err = d.Release(ctx, p, owner); err != nil {
			return provision.Record{}, errors.Join(provision.ErrRetired, err)
		}
		return provision.Record{}, provision.ErrRetired
	}
	return r, err
}

// claim is the same non-expiring conditional gate for active execution and
// address retirement; neither path can steal or refresh another runner's claim.
func (d Dynamo) claim(ctx context.Context, p provision.Plan, owner string) (map[string]types.AttributeValue, error) {
	out, err := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.Table), Key: key(p), UpdateExpression: aws.String("SET #owner = :owner"), ConditionExpression: aws.String("#plan = :plan AND attribute_not_exists(#owner)"),
		ExpressionAttributeNames: map[string]string{"#owner": "Owner", "#plan": "PlanID"}, ExpressionAttributeValues: map[string]types.AttributeValue{":owner": str(owner), ":plan": str(p.ID)}, ReturnValues: types.ReturnValueAllNew,
	})
	if conditional(err) {
		return nil, errors.New("network already has an active runner claim; inspect operation; claims never expire automatically")
	}
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("empty claim response; inspect operation before retrying")
	}
	if value(out.Attributes, "Owner") != owner {
		return nil, errors.New("claim response owner mismatch; inspect operation before retrying")
	}
	return out.Attributes, nil
}
func (d Dynamo) Save(ctx context.Context, r provision.Record, owner string) error {
	if owner == "" {
		return errors.New("empty runner ID")
	}
	if r.Revision < 1 {
		return errors.New("save requires the next journal revision")
	}
	data, err := encode(r)
	if err != nil {
		return err
	}
	return d.save(ctx, r, owner, data)
}

func (d Dynamo) save(ctx context.Context, r provision.Record, owner, data string) error {
	// AWS may commit a write before its acknowledgement is lost. An SDK retry
	// of the exact accepted payload is a no-op, not a conflicting checkpoint.
	// A later revision, different data, different plan or new owner still fails.
	_, err := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(d.Table), Key: key(r.Plan), UpdateExpression: aws.String("SET #data = :data, #revision = :next"), ConditionExpression: aws.String("#plan = :plan AND #owner = :owner AND (#revision = :previous OR (#revision = :next AND #data = :data))"), ExpressionAttributeNames: map[string]string{"#data": "Data", "#plan": "PlanID", "#owner": "Owner", "#revision": "Revision"}, ExpressionAttributeValues: map[string]types.AttributeValue{":data": str(data), ":plan": str(r.Plan.ID), ":owner": str(owner), ":next": number(r.Revision), ":previous": number(r.Revision - 1)}})
	if conditional(err) {
		return errors.New("runner ownership or journal revision changed; stale write refused")
	}
	return err
}
func (d Dynamo) Release(ctx context.Context, p provision.Plan, owner string) error {
	if owner == "" {
		return errors.New("empty runner ID")
	}
	_, err := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(d.Table), Key: key(p), UpdateExpression: aws.String("REMOVE #owner"), ConditionExpression: aws.String("#plan = :plan AND #owner = :owner"), ExpressionAttributeNames: map[string]string{"#plan": "PlanID", "#owner": "Owner"}, ExpressionAttributeValues: map[string]types.AttributeValue{":plan": str(p.ID), ":owner": str(owner)}})
	if conditional(err) {
		return errors.New("runner claim changed or was already released; no change made")
	}
	return err
}
