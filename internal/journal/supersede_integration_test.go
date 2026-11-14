package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

// The handover of a retired network name, against real DynamoDB conditional
// expressions rather than the fake's reading of them. The fixed loopback
// endpoint and explicit disposable flag prevent accidental use against AWS. It
// runs in CI's ephemeral DynamoDB Local container.
func TestJournalDynamoLocalNameReuse(t *testing.T) {
	endpoint := os.Getenv("DASHNET_TEST_DYNAMODB")
	if endpoint == "" {
		t.Skip("requires disposable DynamoDB Local")
	}
	if endpoint != "http://127.0.0.1:8000" || os.Getenv("DASHNET_DISPOSABLE_CI") != "1" {
		t.Fatal("only explicit disposable loopback DynamoDB is supported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, fixture, p, next, cloud := retiredFixture(t)
	retiredItem := clone(fixture.item)
	table := fmt.Sprintf("journal-test-%d", time.Now().UnixNano())
	client := dynamodb.NewFromConfig(aws.Config{
		Region: p.Network.AWS.Region,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "disposable", SecretAccessKey: "disposable"}, nil
		}),
	}, func(o *dynamodb.Options) { o.BaseEndpoint = &endpoint })
	input := &dynamodb.CreateTableInput{
		TableName: aws.String(table), BillingMode: types.BillingModePayPerRequest,
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
		select {
		case <-ready.Done():
			t.Fatal("disposable journal did not start", ready.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := client.DeleteTable(cleanup, &dynamodb.DeleteTableInput{TableName: aws.String(table)}); err != nil {
			t.Error("disposable table cleanup", err)
		}
	})
	store := Dynamo{Client: client, Table: table}
	get := func(network string) map[string]types.AttributeValue {
		t.Helper()
		out, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: map[string]types.AttributeValue{"Network": str(network)}, ConsistentRead: aws.Bool(true)})
		if err != nil {
			t.Fatal(err)
		}
		return out.Item
	}
	put := func(item map[string]types.AttributeValue) {
		t.Helper()
		if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: item}); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := func(want map[string]types.AttributeValue) bool {
		got := get(p.Key())
		return len(got) == len(want) && value(got, "PlanID") == value(want, "PlanID") && value(got, "Data") == value(want, "Data") && value(got, "Owner") == value(want, "Owner") && sameNumber(got["Revision"], want["Revision"]) && len(get(p.ArchiveKey())) == 0
	}

	t.Run("refused while claimed or not retired", func(t *testing.T) {
		claimed := clone(retiredItem)
		claimed["Owner"] = str("another-runner")
		put(claimed)
		if _, err := store.Supersede(ctx, next, "runner"); err == nil || !unchanged(claimed) {
			t.Fatal("claimed retired operation superseded", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value(retiredItem, "Data")), &fields); err != nil {
			t.Fatal(err)
		}
		fields["phase"] = json.RawMessage(`"compute-ready"`)
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		running := clone(retiredItem)
		running["Data"] = str(string(data))
		put(running)
		if _, _, err = store.Succession(ctx, p); err != nil {
			t.Fatal("a live network no longer plans as itself", err)
		}
		if _, err = store.Supersede(ctx, next, "runner"); err == nil || !unchanged(running) {
			t.Fatal("live operation superseded", err)
		}
		put(retiredItem)
		if _, err = store.Acquire(ctx, next, "bootstrap"); err == nil || !unchanged(retiredItem) {
			t.Fatal("a stage other than provision took the name over", err)
		}
	})

	n := p.Network
	n.Chain.Generation++
	base, err := provision.Prepare(ctx, n, testutil.Identity{Account: n.AWS.AccountID}, cloud)
	if err != nil {
		t.Fatal(err)
	}
	rival, err := base.Successor(p.ID)
	if err != nil || rival.ID == next.ID {
		t.Fatal("rival successor invalid", err)
	}
	var winner provision.Plan
	var record provision.Record
	owner := ""

	t.Run("competing successors and the kept archive", func(t *testing.T) {
		if succeeds, reused, err := store.Succession(ctx, p); err != nil || succeeds != p.ID || !reused {
			t.Fatal("retired operation not recognised", err)
		}
		type result struct {
			plan   provision.Plan
			owner  string
			record provision.Record
			err    error
		}
		results := make(chan result, 6)
		var wg sync.WaitGroup
		for j, plan := range []provision.Plan{next, rival, next, rival, next, rival} {
			runner := fmt.Sprint("runner-", j)
			wg.Go(func() {
				r, err := store.Supersede(ctx, plan, runner)
				results <- result{plan, runner, r, err}
			})
		}
		wg.Wait()
		close(results)
		wins := 0
		for r := range results {
			if r.err == nil {
				winner, owner, record = r.plan, r.owner, r.record
				wins++
			}
		}
		if wins != 1 {
			t.Fatal("handover was not exclusive", wins)
		}
		live, kept := get(p.Key()), get(p.ArchiveKey())
		if value(live, "PlanID") != winner.ID || value(live, "Owner") != owner || !sameNumber(live["Revision"], number(0)) {
			t.Fatal("live item is not the winner's claimed operation")
		}
		if len(kept) != 4 || value(kept, "PlanID") != p.ID || value(kept, "Data") != value(retiredItem, "Data") || !sameNumber(kept["Revision"], retiredItem["Revision"]) {
			t.Fatal("retired operation was not archived verbatim")
		}
		archived, err := store.Archived(ctx, p)
		if err != nil || archived.Phase != "addresses-released" || archived.Plan.ID != p.ID {
			t.Fatal("archived operation unreadable", err)
		}
		if record.Predecessor == nil || record.Predecessor.PlanID != p.ID || record.Predecessor.Archive != p.ArchiveKey() {
			t.Fatal("successor lost its predecessor")
		}
	})

	t.Run("successor is an ordinary operation and the loser stays refused", func(t *testing.T) {
		loser := rival
		if winner.ID == rival.ID {
			loser = next
		}
		record.Phase, record.LastRunner = "provisioning", owner
		record.Revision++
		if err := store.Save(ctx, record, owner); err != nil {
			t.Fatal(err)
		}
		if err := store.Release(ctx, winner, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Supersede(ctx, loser, "late"); err == nil || value(get(p.Key()), "PlanID") != winner.ID || value(get(p.Key()), "Owner") != "" {
			t.Fatal("losing plan replaced the winner", err)
		}
		for _, acquire := range []func(context.Context, provision.Plan, string) (provision.Record, error){store.Supersede, store.Acquire} {
			resumed, err := acquire(ctx, winner, "resume")
			if err != nil || resumed.Phase != "provisioning" || resumed.Revision != 1 {
				t.Fatal("successor operation did not resume", err)
			}
			if err = store.Release(ctx, winner, "resume"); err != nil {
				t.Fatal(err)
			}
		}
		if value(get(p.ArchiveKey()), "Data") != value(retiredItem, "Data") {
			t.Fatal("archive changed after the handover")
		}
		if _, _, err := store.Read(ctx, p); err == nil {
			t.Fatal("retired plan still reads the live operation")
		}
		if _, err := store.Supersede(ctx, p, "late"); err == nil {
			t.Fatal("a plan that succeeds nothing superseded an operation")
		}
	})
}
