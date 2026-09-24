package managed

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
	"github.com/dashpay/dash-network-go/internal/journal"
)

type Record struct {
	FleetID     string          `json:"fleetId"`
	SnapshotID  string          `json:"snapshotId"`
	Revision    int64           `json:"revision"`
	Enrolled    map[string]bool `json:"enrolled"`
	OperationID string          `json:"operationId,omitempty"`
	Phase       string          `json:"phase"`
	Current     string          `json:"current,omitempty"`
	Completed   map[string]bool `json:"completed,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
}

func (r Record) Validate(f Fleet) error {
	if r.FleetID != f.ID() || !digestRE.MatchString(r.SnapshotID) || r.Revision < 0 || len(r.Enrolled) != len(f.Targets) {
		return errors.New("managed record scope mismatch")
	}
	if r.OperationID != "" && !digestRE.MatchString(r.OperationID) {
		return errors.New("invalid managed operation ID")
	}
	if r.Phase != "enrolling" && r.Phase != "enrolled" && r.Phase != "staging" && r.Phase != "applying" && r.Phase != "verifying" && r.Phase != "interrupted" && r.Phase != "complete" {
		return errors.New("invalid managed phase")
	}
	found := r.Current == ""
	for _, t := range f.Targets {
		if _, ok := r.Enrolled[t.Name]; !ok {
			return errors.New("managed record lost target")
		}
		if r.OperationID != "" && !r.Enrolled[t.Name] {
			return errors.New("unenrolled target in operation")
		}
		if t.Name == r.Current {
			found = true
		}
		if r.Phase == "complete" && !r.Completed[t.Name] {
			return errors.New("incomplete target progress")
		}
	}
	if !found || (r.Phase == "complete" && r.Current != "") {
		return errors.New("invalid pending target")
	}
	return nil
}

type Store interface {
	Read(context.Context, Fleet) (Record, string, error)
	Acquire(context.Context, Fleet, Snapshot, string) (Record, error)
	Save(context.Context, Fleet, Record, string) error
	Release(context.Context, Fleet, string) error
}
type Dynamo struct {
	Client journal.API
	Table  string
}

func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func num(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}
func value(m map[string]types.AttributeValue, k string) string {
	v, _ := m[k].(*types.AttributeValueMemberS)
	if v == nil {
		return ""
	}
	return v.Value
}
func key(f Fleet) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"Network": str(f.Key())}
}
func conditional(e error) bool { var v *types.ConditionalCheckFailedException; return errors.As(e, &v) }
func (d Dynamo) Verify(ctx context.Context, f Fleet) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if d.Table != f.StateTable {
		return errors.New("journal scope mismatch")
	}
	o, e := d.Client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(d.Table)})
	if e != nil {
		return e
	}
	if o == nil || o.Table == nil {
		return errors.New("missing journal table")
	}
	t := o.Table
	if aws.ToString(t.TableArn) != "arn:aws:dynamodb:"+f.Region+":"+f.AccountID+":table/"+f.StateTable || t.TableStatus != types.TableStatusActive || len(t.KeySchema) != 1 || aws.ToString(t.KeySchema[0].AttributeName) != "Network" || t.KeySchema[0].KeyType != types.KeyTypeHash {
		return errors.New("wrong account/region/schema for journal")
	}
	for _, a := range t.AttributeDefinitions {
		if aws.ToString(a.AttributeName) == "Network" && a.AttributeType == types.ScalarAttributeTypeS {
			return nil
		}
	}
	return errors.New("Network partition key must be string")
}
func decode(f Fleet, item map[string]types.AttributeValue) (Record, error) {
	var r Record
	if value(item, "Network") != f.Key() || value(item, "Kind") != "ExistingNetwork" || value(item, "FleetID") != f.ID() {
		return r, errors.New("network already managed by another manifest/engine; no automatic adoption")
	}
	raw := value(item, "Data")
	if len(raw) > 300000 {
		return r, errors.New("journal too large")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, err
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return r, errors.New("extra journal data")
	}
	n, ok := item["Revision"].(*types.AttributeValueMemberN)
	if !ok || n.Value != strconv.FormatInt(r.Revision, 10) {
		return r, errors.New("journal revision mismatch")
	}
	return r, r.Validate(f)
}
func (d Dynamo) Read(ctx context.Context, f Fleet) (Record, string, error) {
	o, e := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: key(f), ConsistentRead: aws.Bool(true)})
	if e != nil {
		return Record{}, "", e
	}
	if o == nil || len(o.Item) == 0 {
		return Record{}, "", journal.ErrNotFound
	}
	r, e := decode(f, o.Item)
	return r, value(o.Item, "Owner"), e
}
func (d Dynamo) Acquire(ctx context.Context, f Fleet, s Snapshot, owner string) (Record, error) {
	if owner == "" {
		return Record{}, errors.New("empty owner")
	}
	r := Record{FleetID: f.ID(), SnapshotID: s.ID, Phase: "enrolling", Enrolled: map[string]bool{}}
	for _, t := range f.Targets {
		r.Enrolled[t.Name] = false
	}
	if e := r.Validate(f); e != nil {
		return r, e
	}
	b, _ := json.Marshal(r)
	item := key(f)
	item["Kind"] = str("ExistingNetwork")
	item["FleetID"] = str(f.ID())
	item["Data"] = str(string(b))
	item["Revision"] = num(0)
	item["Owner"] = str(owner)
	_, e := d.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(d.Table), Item: item, ConditionExpression: aws.String("attribute_not_exists(#network)"), ExpressionAttributeNames: map[string]string{"#network": "Network"}})
	if e == nil {
		return r, nil
	}
	if !conditional(e) {
		return r, e
	}
	if _, _, e = d.Read(ctx, f); e != nil {
		return r, e
	}
	o, e := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(d.Table), Key: key(f), UpdateExpression: aws.String("SET #owner = :owner"), ConditionExpression: aws.String("#fleet = :fleet AND #kind = :kind AND attribute_not_exists(#owner)"), ExpressionAttributeNames: map[string]string{"#owner": "Owner", "#fleet": "FleetID", "#kind": "Kind"}, ExpressionAttributeValues: map[string]types.AttributeValue{":owner": str(owner), ":fleet": str(f.ID()), ":kind": str("ExistingNetwork")}, ReturnValues: types.ReturnValueAllNew})
	if conditional(e) {
		return r, errors.New("network already has a runner claim; stop and inspect before unlock")
	}
	if e != nil {
		return r, e
	}
	if o == nil {
		return r, errors.New("claim response lost")
	}
	return decode(f, o.Attributes)
}
func (d Dynamo) Save(ctx context.Context, f Fleet, r Record, owner string) error {
	if e := r.Validate(f); e != nil {
		return e
	}
	if owner == "" || r.Revision < 1 {
		return errors.New("owner and next revision required")
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) > 300000 {
		return errors.New("journal exceeds budget")
	}
	_, e = d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(d.Table), Key: key(f), UpdateExpression: aws.String("SET #data = :data, #revision = :next"), ConditionExpression: aws.String("#fleet = :fleet AND #owner = :owner AND (#revision = :previous OR (#revision = :next AND #data = :data))"), ExpressionAttributeNames: map[string]string{"#data": "Data", "#revision": "Revision", "#fleet": "FleetID", "#owner": "Owner"}, ExpressionAttributeValues: map[string]types.AttributeValue{":fleet": str(f.ID()), ":owner": str(owner), ":data": str(string(b)), ":next": num(r.Revision), ":previous": num(r.Revision - 1)}})
	if conditional(e) {
		return errors.New("stale revision or lost runner claim")
	}
	return e
}
func (d Dynamo) Release(ctx context.Context, f Fleet, owner string) error {
	if owner == "" {
		return errors.New("empty owner")
	}
	_, e := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(d.Table), Key: key(f), UpdateExpression: aws.String("REMOVE #owner"), ConditionExpression: aws.String("#fleet = :fleet AND #owner = :owner"), ExpressionAttributeNames: map[string]string{"#fleet": "FleetID", "#owner": "Owner"}, ExpressionAttributeValues: map[string]types.AttributeValue{":fleet": str(f.ID()), ":owner": str(owner)}})
	if conditional(e) {
		return fmt.Errorf("claim changed; no unlock performed")
	}
	return e
}
