package managed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/dashpay/dash-network-go/internal/journal"
)

// This test uses real DynamoDB conditional expressions, not a mock expression
// interpreter. The fixed loopback endpoint and explicit disposable flag prevent
// accidental use against AWS. It runs in CI's ephemeral DynamoDB Local container.
func TestManagedDynamoLocalClaimsAndRecovery(t *testing.T) {
	endpoint := os.Getenv("DASHNET_TEST_DYNAMODB")
	if endpoint == "" {
		t.Skip("requires disposable DynamoDB Local")
	}
	if endpoint != "http://127.0.0.1:8000" || os.Getenv("DASHNET_DISPOSABLE_CI") != "1" {
		t.Fatal("only explicit disposable loopback DynamoDB is supported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, _, _, _ := setup(t)
	s.Fleet.StateTable = fmt.Sprintf("managed-test-%d", time.Now().UnixNano())
	s.ID = ""
	s.ID = hash(s)
	f := s.Fleet
	client := dynamodb.NewFromConfig(aws.Config{
		Region: f.Region,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "disposable", SecretAccessKey: "disposable"}, nil
		}),
	}, func(o *dynamodb.Options) { o.BaseEndpoint = &endpoint })
	input := &dynamodb.CreateTableInput{
		TableName: aws.String(f.StateTable), BillingMode: types.BillingModePayPerRequest,
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("Network"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("Network"), AttributeType: types.ScalarAttributeTypeS}},
	}
	ready, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	for {
		if _, err := client.CreateTable(ready, input); err == nil {
			break
		} else if ready.Err() != nil {
			t.Fatal("disposable journal did not start", err)
		}
		if err := sleep(ready, 200*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := client.DeleteTable(cleanup, &dynamodb.DeleteTableInput{TableName: aws.String(f.StateTable)}); err != nil {
			t.Error("disposable table cleanup", err)
		}
	})
	store := Dynamo{Client: client, Table: f.StateTable}

	t.Run("competing runners and accepted save replay", func(t *testing.T) {
		type result struct {
			owner  string
			record Record
			err    error
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for _, owner := range []string{"one", "two"} {
			wg.Go(func() {
				r, err := store.Acquire(ctx, f, s, owner)
				results <- result{owner, r, err}
			})
		}
		wg.Wait()
		close(results)
		var winner result
		count := 0
		for r := range results {
			if r.err == nil {
				winner = r
				count++
			}
		}
		if count != 1 {
			t.Fatal("claim did not have exactly one winner", count)
		}
		r := winner.record
		r.Revision++
		r.Enrolled[f.Targets[0].Name] = true
		lost := Dynamo{Client: &lostManagedResponse{API: client, update: true}, Table: f.StateTable}
		if err := lost.Save(ctx, f, r, winner.owner); err == nil {
			t.Fatal("expected accepted save response loss")
		}
		if err := store.Save(ctx, f, r, winner.owner); err != nil {
			t.Fatal("identical accepted save could not replay", err)
		}
		old := clone(r)
		r.Revision++
		r.Enrolled[f.Targets[1].Name] = true
		if err := store.Save(ctx, f, r, winner.owner); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(ctx, f, old, winner.owner); err == nil {
			t.Fatal("late old response rolled back checkpoint")
		}
		if err := store.Release(ctx, f, "foreign"); err == nil {
			t.Fatal("foreign runner released owner")
		}
		if err := store.Release(ctx, f, winner.owner); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(ctx, f, s, "successor"); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(ctx, f, r, winner.owner); err == nil {
			t.Fatal("stale owner saved after transfer")
		}
		got, owner, err := store.Read(ctx, f)
		if err != nil || owner != "successor" || got.Revision != 2 || !got.Enrolled[f.Targets[1].Name] {
			t.Fatal("checkpoint/owner not preserved", err)
		}
	})

	t.Run("ambiguous create stays claimed", func(t *testing.T) {
		other := clone(s)
		other.Fleet.Metadata.Name = "devnet-lost"
		other.ID = ""
		other.ID = hash(other)
		f := other.Fleet
		lost := Dynamo{Client: &lostManagedResponse{API: client, put: true}, Table: f.StateTable}
		if _, err := lost.Acquire(ctx, f, other, "lost"); err == nil {
			t.Fatal("expected accepted claim response loss")
		}
		if _, err := store.Acquire(ctx, f, other, "replacement"); err == nil {
			t.Fatal("ambiguous claim was stolen")
		}
		_, owner, err := store.Read(ctx, f)
		if err != nil || owner != "lost" {
			t.Fatal("ambiguous owner evidence lost", err)
		}
	})

	t.Run("native record is not adopted", func(t *testing.T) {
		other := clone(s)
		other.Fleet.Metadata.Name = "devnet-native"
		other.ID = ""
		other.ID = hash(other)
		f := other.Fleet
		item := key(f)
		item["PlanID"] = str(h64("native provisioning plan"))
		if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(f.StateTable), Item: item}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(ctx, f, other, "adopter"); err == nil {
			t.Fatal("native network was implicitly adopted")
		}
		got, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(f.StateTable), Key: key(f), ConsistentRead: aws.Bool(true)})
		if err != nil || len(got.Item) != 2 || value(got.Item, "PlanID") != h64("native provisioning plan") {
			t.Fatal("native record was changed", err)
		}
	})
}

type lostManagedResponse struct {
	journal.API
	put, update bool
}

func (l *lostManagedResponse) PutItem(ctx context.Context, in *dynamodb.PutItemInput, opt ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	out, err := l.API.PutItem(ctx, in, opt...)
	if err == nil && l.put {
		l.put = false
		return nil, errors.New("accepted claim response lost")
	}
	return out, err
}
func (l *lostManagedResponse) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opt ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	out, err := l.API.UpdateItem(ctx, in, opt...)
	if err == nil && l.update {
		l.update = false
		return nil, errors.New("accepted checkpoint response lost")
	}
	return out, err
}
