// Package journal stores private operation state and non-expiring runner claims.
// The table is provisioned once out-of-band; this package never creates/deletes it.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if value(item, "Network") != p.Key() || value(item, "PlanID") != p.ID {
		return provision.Record{}, errors.New("network is already bound to a different plan; no automatic reset, replacement, or state deletion")
	}
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
	return r, r.Validate(p)
}

// Read is strongly consistent so a resume never relies on an eventually
// consistent lock read. Conditional writes remain the actual exclusion gate.
func (d Dynamo) Read(ctx context.Context, p provision.Plan) (provision.Record, string, error) {
	out, err := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: key(p), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return provision.Record{}, "", err
	}
	if out == nil || len(out.Item) == 0 {
		return provision.Record{}, "", ErrNotFound
	}
	r, err := decode(out.Item, p)
	return r, value(out.Item, "Owner"), err
}
func (d Dynamo) Acquire(ctx context.Context, p provision.Plan, owner string) (provision.Record, error) {
	if owner == "" {
		return provision.Record{}, errors.New("empty runner ID")
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
	_, err = d.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(d.Table), Item: item, ConditionExpression: aws.String("attribute_not_exists(#network)"), ExpressionAttributeNames: map[string]string{"#network": "Network"}})
	if err == nil {
		return r, nil
	}
	if !conditional(err) {
		return r, err
	}
	// Do not overwrite an existing record. It may be an interrupted operation.
	if _, _, err = d.Read(ctx, p); err != nil {
		return r, err
	}
	out, err := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.Table), Key: key(p), UpdateExpression: aws.String("SET #owner = :owner"), ConditionExpression: aws.String("#plan = :plan AND attribute_not_exists(#owner)"),
		ExpressionAttributeNames: map[string]string{"#owner": "Owner", "#plan": "PlanID"}, ExpressionAttributeValues: map[string]types.AttributeValue{":owner": str(owner), ":plan": str(p.ID)}, ReturnValues: types.ReturnValueAllNew,
	})
	if conditional(err) {
		return r, errors.New("network already has an active runner claim; inspect operation; claims never expire automatically")
	}
	if err != nil {
		return r, err
	}
	if out == nil {
		return r, errors.New("empty claim response; inspect operation before retrying")
	}
	return decode(out.Attributes, p)
}
func (d Dynamo) Save(ctx context.Context, r provision.Record, owner string) error {
	if owner == "" {
		return errors.New("empty runner ID")
	}
	data, err := encode(r)
	if err != nil {
		return err
	}
	_, err = d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(d.Table), Key: key(r.Plan), UpdateExpression: aws.String("SET #data = :data"), ConditionExpression: aws.String("#plan = :plan AND #owner = :owner"), ExpressionAttributeNames: map[string]string{"#data": "Data", "#plan": "PlanID", "#owner": "Owner"}, ExpressionAttributeValues: map[string]types.AttributeValue{":data": str(data), ":plan": str(r.Plan.ID), ":owner": str(owner)}})
	if conditional(err) {
		return errors.New("lost runner ownership; journal write refused")
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
