package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

// retiredFixture is a destroyed network as the journal really holds one: the
// historical fleet of cleanupFixture, whose application history today's rules
// reject, after release-addresses retired it. next is the plan provision-plan
// then produces for the same, unchanged network definition.
func retiredFixture(t *testing.T) (Dynamo, *database, provision.Plan, provision.Plan, *cleanupCloud) {
	t.Helper()
	d, db, p, c := cleanupFixture(t)
	ctx := context.Background()
	// While the fleet has its addresses, the network plans as itself.
	if succeeds, reused, err := d.Succession(ctx, p); err != nil || succeeds != "" || reused {
		t.Fatal("a network that still has its addresses was offered for reuse", err)
	}
	if r, err := provision.ReleaseAddresses(ctx, p, testutil.Identity{Account: p.Network.AWS.AccountID}, c, d.AddressCleanup(), "destroy"); err != nil || r.Phase != "addresses-released" {
		t.Fatal("fixture was not retired", err)
	}
	succeeds, reused, err := d.Succession(ctx, p)
	if err != nil || succeeds != p.ID || !reused {
		t.Fatal("retired operation not recognised", err)
	}
	next, err := p.Successor(succeeds)
	if err != nil || next.ID == p.ID || next.Key() != p.Key() {
		t.Fatal("invalid successor plan", err)
	}
	return d, db, p, next, c
}

// rewrite changes the stored payload of the live item, as raw JSON.
func rewrite(t *testing.T, db *database, change func(map[string]json.RawMessage)) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value(db.item, "Data")), &fields); err != nil {
		t.Fatal(err)
	}
	change(fields)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	db.item["Data"] = str(string(data))
}

func TestRetiredNameIsArchivedAndHandedToItsSuccessor(t *testing.T) {
	d, db, p, next, _ := retiredFixture(t)
	ctx := context.Background()
	before := clone(db.item)
	// No stage but provision hands a name over, and the retired plan stays retired.
	if _, err := d.Acquire(ctx, next, "bootstrap"); err == nil || value(db.item, "PlanID") != p.ID || value(db.item, "Owner") != "" || len(db.archived) != 0 {
		t.Fatal("a stage other than provision took the name over", err)
	}
	if _, err := d.Supersede(ctx, p, "retired"); err == nil {
		t.Fatal("a plan that succeeds nothing superseded an operation")
	}
	r, err := d.Supersede(ctx, next, "one")
	if err != nil || r.Phase != "pending" || r.Revision != 0 || r.Plan.ID != next.ID || r.Predecessor == nil || r.Predecessor.PlanID != p.ID || len(r.Predecessor.Instances) != len(p.Targets) || r.Bootstrap != nil || r.Deployment != nil || r.Runtime != nil || r.Upgrade != nil {
		t.Fatal("successor did not start a fresh operation", err)
	}
	kept := db.archived[p.ArchiveKey()]
	if len(db.archived) != 1 || len(kept) != 4 || value(kept, "Network") != p.ArchiveKey() || value(kept, "PlanID") != p.ID || value(kept, "Data") != value(before, "Data") || !sameNumber(kept["Revision"], before["Revision"]) {
		t.Fatal("retired operation was not archived verbatim")
	}
	if value(db.item, "Network") != p.Key() || value(db.item, "PlanID") != next.ID || value(db.item, "Owner") != "one" || !sameNumber(db.item["Revision"], number(0)) {
		t.Fatal("live item is not the successor's claimed operation")
	}
	current, owner, err := d.Read(ctx, next)
	if err != nil || owner != "one" || current.Predecessor.Archive != p.ArchiveKey() || current.Predecessor.Revision != r.Predecessor.Revision {
		t.Fatal("successor operation unreadable", err)
	}
	if _, _, err = d.Read(ctx, p); err == nil {
		t.Fatal("retired plan still reads the live operation")
	}
	archived, err := d.Archived(ctx, p)
	if err != nil || archived.Phase != "addresses-released" || archived.Plan.ID != p.ID || len(archived.Nodes) != len(p.Targets) {
		t.Fatal("archived operation unreadable", err)
	}
	if _, err = d.Archived(ctx, next); !errors.Is(err, ErrNotFound) {
		t.Fatal("live operation reported as archived", err)
	}
	// The retired plan can no longer be cleaned up, claimed or unlocked: its key is not its own.
	if _, err = d.AddressCleanup().Acquire(ctx, p, "cleanup"); err == nil || value(db.item, "Owner") != "one" {
		t.Fatal("retired plan claimed its successor's operation", err)
	}
	// From here on it is an ordinary operation, for provision and every later stage.
	r.Phase, r.LastRunner = "provisioning", "one"
	r.Revision++
	if err = d.Save(ctx, r, "one"); err != nil {
		t.Fatal(err)
	}
	if err = d.Release(ctx, next, "one"); err != nil {
		t.Fatal(err)
	}
	for _, acquire := range []func(context.Context, provision.Plan, string) (provision.Record, error){d.Supersede, d.Acquire} {
		resumed, err := acquire(ctx, next, "two")
		if err != nil || resumed.Phase != "provisioning" || resumed.Revision != 1 || len(db.archived) != 1 || db.replaced != 1 {
			t.Fatal("successor operation did not resume", err)
		}
		if err = d.Release(ctx, next, "two"); err != nil {
			t.Fatal(err)
		}
	}
	// Planning the same network again yields the same successor plan, not a new one.
	succeeds, reused, err := d.Succession(ctx, p)
	if err != nil || succeeds != p.ID || reused {
		t.Fatal("an interrupted successor cannot be planned again", err)
	}
	if value(db.archived[p.ArchiveKey()], "Data") != value(before, "Data") {
		t.Fatal("archive changed after the handover")
	}
}

func TestOnlyARetiredUnclaimedOperationIsSuperseded(t *testing.T) {
	phase := func(name string) func(*testing.T, *database) {
		return func(t *testing.T, db *database) {
			rewrite(t, db, func(fields map[string]json.RawMessage) { fields["phase"] = json.RawMessage(`"` + name + `"`) })
		}
	}
	node := func(change func(map[string]any)) func(*testing.T, *database) {
		return func(t *testing.T, db *database) {
			rewrite(t, db, func(fields map[string]json.RawMessage) {
				var nodes map[string]map[string]any
				if err := json.Unmarshal(fields["nodes"], &nodes); err != nil {
					t.Fatal(err)
				}
				change(nodes["wallet-001"])
				fields["nodes"], _ = json.Marshal(nodes)
			})
		}
	}
	cases := map[string]func(*testing.T, *database){
		"pending":       phase("pending"),
		"provisioning":  phase("provisioning"),
		"interrupted":   phase("interrupted"),
		"compute-ready": phase("compute-ready"),
		"unknown-phase": phase("destroyed"),
		"claimed":       func(_ *testing.T, db *database) { db.item["Owner"] = str("another-runner") },
		"address-kept":  node(func(n map[string]any) { n["address"].(map[string]any)["phase"] = "releasing" }),
		"no-instance":   node(func(n map[string]any) { delete(n, "instanceId") }),
		"not-ours":      func(_ *testing.T, db *database) { db.item["Kind"] = str("ExistingNetwork") },
		"revision":      func(_ *testing.T, db *database) { db.item["Revision"] = number(1) },
		"other-network": func(_ *testing.T, db *database) { db.item["Network"] = str("000000000000/us-west-2/devnet-other") },
		"corrupt":       func(_ *testing.T, db *database) { db.item["Data"] = str(value(db.item, "Data") + " {}") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d, db, p, next, _ := retiredFixture(t)
			ctx := context.Background()
			change(t, db)
			before := clone(db.item)
			// Planning never offers the name: at most the plan the journal already holds.
			if succeeds, reused, err := d.Succession(ctx, p); (err != nil && !errors.Is(err, ErrNotRetired)) || succeeds != "" || reused {
				t.Fatal("unretired operation offered for reuse", err)
			}
			if _, err := d.Supersede(ctx, next, "runner"); err == nil {
				t.Fatal("unretired operation superseded")
			}
			if len(db.archived) != 0 || db.replaced != 0 || len(db.item) != len(before) || value(db.item, "Data") != value(before, "Data") || value(db.item, "PlanID") != p.ID || value(db.item, "Owner") != value(before, "Owner") {
				t.Fatal("refused handover changed the journal")
			}
		})
	}
	t.Run("another-predecessor", func(t *testing.T) {
		d, db, p, _, _ := retiredFixture(t)
		other, err := p.Successor(strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Supersede(context.Background(), other, "runner"); err == nil || len(db.archived) != 0 || value(db.item, "PlanID") != p.ID {
			t.Fatal("operation superseded by a plan that does not name it", err)
		}
	})
	t.Run("free-name", func(t *testing.T) {
		d, db, p := setup(t)
		ctx := context.Background()
		if succeeds, reused, err := d.Succession(ctx, p); err != nil || succeeds != "" || reused {
			t.Fatal("free name has a succession", err)
		}
		next, err := p.Successor(strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		for _, acquire := range []func(context.Context, provision.Plan, string) (provision.Record, error){d.Supersede, d.Acquire} {
			if _, err = acquire(ctx, next, "runner"); err == nil || db.item != nil || len(db.archived) != 0 {
				t.Fatal("successor plan created an operation with nothing to succeed", err)
			}
		}
		// A live first operation plans as itself, and a changed network is told so.
		if _, err = d.Acquire(ctx, p, "runner"); err != nil {
			t.Fatal(err)
		}
		if succeeds, reused, err := d.Succession(ctx, p); err != nil || succeeds != "" || reused {
			t.Fatal("live operation cannot be planned again", err)
		}
		if _, _, err = d.Succession(ctx, next); err == nil {
			t.Fatal("succession computed from a successor plan")
		}
		n := p.Network
		n.Chain.Generation++
		changed, err := provision.Prepare(ctx, n, testutil.Identity{Account: n.AWS.AccountID}, &testutil.Cloud{Network: n})
		if err != nil || changed.ID == p.ID {
			t.Fatal(err)
		}
		if succeeds, reused, err := d.Succession(ctx, changed); !errors.Is(err, ErrNotRetired) || succeeds != "" || reused {
			t.Fatal("a changed network was offered a live network's name", err)
		}
	})
}

func TestConcurrentSupersedeHasExactlyOneWinner(t *testing.T) {
	d, db, p, next, c := retiredFixture(t)
	ctx := context.Background()
	// A second, different successor of the same retired plan competes too.
	n := p.Network
	n.Chain.Generation++
	base, err := provision.Prepare(ctx, n, testutil.Identity{Account: n.AWS.AccountID}, c)
	if err != nil {
		t.Fatal(err)
	}
	rival, err := base.Successor(p.ID)
	if err != nil || rival.ID == next.ID || rival.Key() != next.Key() {
		t.Fatal("rival successor invalid", err)
	}
	type result struct {
		plan provision.Plan
		err  error
	}
	results := make(chan result, 4)
	var wg sync.WaitGroup
	for j, plan := range []provision.Plan{next, next, rival, rival} {
		owner := fmt.Sprint("runner-", j)
		wg.Go(func() {
			_, err := d.Supersede(ctx, plan, owner)
			results <- result{plan, err}
		})
	}
	wg.Wait()
	close(results)
	var winner provision.Plan
	wins := 0
	for r := range results {
		if r.err == nil {
			winner = r.plan
			wins++
		}
	}
	if wins != 1 || db.replaced != 1 || value(db.item, "PlanID") != winner.ID || value(db.item, "Owner") == "" || len(db.archived) != 1 || value(db.archived[p.ArchiveKey()], "PlanID") != p.ID {
		t.Fatal("handover was not exclusive", wins, db.replaced)
	}
	// The loser's plan stays refused: the name now belongs to the winner.
	loser := rival
	if winner.ID == rival.ID {
		loser = next
	}
	if err = d.Release(ctx, winner, value(db.item, "Owner")); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Supersede(ctx, loser, "late"); err == nil || value(db.item, "PlanID") != winner.ID || value(db.item, "Owner") != "" {
		t.Fatal("losing plan replaced the winner", err)
	}
}

func TestHandoverSurvivesLostResponsesAndRepeats(t *testing.T) {
	t.Run("stopped-after-archive", func(t *testing.T) {
		d, db, p, next, _ := retiredFixture(t)
		ctx := context.Background()
		before := value(db.item, "Data")
		db.replace = "refuse"
		if _, err := d.Supersede(ctx, next, "first"); err == nil || db.replaced != 0 || value(db.item, "PlanID") != p.ID || value(db.item, "Owner") != "" || len(db.archived) != 1 {
			t.Fatal("interrupted handover left an unexpected journal", err)
		}
		// Still retired, still plannable, and the archive is not written twice.
		if succeeds, reused, err := d.Succession(ctx, p); err != nil || succeeds != p.ID || !reused {
			t.Fatal("retired operation lost after an interrupted handover", err)
		}
		db.replace = ""
		if _, err := d.Supersede(ctx, next, "second"); err != nil || db.replaced != 1 || len(db.archived) != 1 || value(db.archived[p.ArchiveKey()], "Data") != before || value(db.item, "Owner") != "second" {
			t.Fatal("repeat after an interrupted handover failed", err)
		}
	})
	t.Run("archive-holds-something-else", func(t *testing.T) {
		d, db, p, next, _ := retiredFixture(t)
		db.archived = map[string]map[string]types.AttributeValue{p.ArchiveKey(): {"Network": str(p.ArchiveKey()), "PlanID": str(p.ID), "Data": str("{}"), "Revision": number(1)}}
		if _, err := d.Supersede(context.Background(), next, "runner"); err == nil || db.replaced != 0 || value(db.item, "PlanID") != p.ID || value(db.archived[p.ArchiveKey()], "Data") != "{}" {
			t.Fatal("handover overwrote or ignored different archived evidence", err)
		}
	})
	t.Run("response-lost", func(t *testing.T) {
		d, db, p, next, _ := retiredFixture(t)
		ctx := context.Background()
		db.replace = "lose"
		if _, err := d.Supersede(ctx, next, "lost"); err == nil || db.replaced != 1 {
			t.Fatal("expected an ambiguous handover", err)
		}
		// Like a lost Acquire response: the claim exists and is never stolen.
		if _, err := d.Supersede(ctx, next, "new-runner"); err == nil || value(db.item, "Owner") != "lost" {
			t.Fatal("ambiguous handover claim was stolen", err)
		}
		current, owner, err := d.Read(ctx, next)
		if err != nil || owner != "lost" || current.Phase != "pending" || current.Predecessor.PlanID != p.ID {
			t.Fatal("ambiguous handover is not inspectable", err)
		}
		// The operator unlocks the stopped runner; the same plan then resumes.
		if err = d.Release(ctx, next, "lost"); err != nil {
			t.Fatal(err)
		}
		if _, err = d.Supersede(ctx, next, "new-runner"); err != nil || db.replaced != 1 || len(db.archived) != 1 {
			t.Fatal("handover did not resume after unlock", err)
		}
	})
	t.Run("own-write-retried", func(t *testing.T) {
		d, db, p, next, _ := retiredFixture(t)
		db.replace = "retry"
		r, err := d.Supersede(context.Background(), next, "runner")
		if err != nil || r.Predecessor == nil || r.Predecessor.PlanID != p.ID || db.replaced != 1 || value(db.item, "Owner") != "runner" || value(db.item, "PlanID") != next.ID {
			t.Fatal("a retry of the accepted handover was treated as a conflict", err)
		}
	})
}

func TestSuccessorIsItselfRetiredAndSucceeded(t *testing.T) {
	d, db, p, next, _ := retiredFixture(t)
	ctx := context.Background()
	r, err := d.Supersede(ctx, next, "runner")
	if err != nil {
		t.Fatal(err)
	}
	// The second fleet is provisioned, destroyed and retired in its turn.
	for j, target := range next.Targets {
		at := r.CreatedAt
		r.Nodes[target.Name] = provision.NodeProgress{Phase: "present", AttemptedAt: at, ObservedAt: at, EC2State: "terminated", InstanceID: fmt.Sprintf("i-%017x", 100+j), Address: &provision.AddressProgress{Phase: "released", AttemptedAt: at, AllocationID: fmt.Sprintf("eipalloc-%017x", 100+j), PublicIP: fmt.Sprintf("198.51.100.%d", 100+j)}}
	}
	r.Phase = "addresses-released"
	r.Revision++
	if err = d.Save(ctx, r, "runner"); err != nil {
		t.Fatal(err)
	}
	if err = d.Release(ctx, next, "runner"); err != nil {
		t.Fatal(err)
	}
	succeeds, reused, err := d.Succession(ctx, p)
	if err != nil || succeeds != next.ID || !reused {
		t.Fatal("retired successor not recognised", err)
	}
	third, err := p.Successor(succeeds)
	if err != nil || third.ID == next.ID || third.ID == p.ID {
		t.Fatal("second successor repeats an earlier plan", err)
	}
	fresh, err := d.Supersede(ctx, third, "runner")
	if err != nil || fresh.Predecessor.PlanID != next.ID || len(fresh.Predecessor.Instances) != len(next.Targets) || value(db.item, "PlanID") != third.ID {
		t.Fatal("second handover failed", err)
	}
	// Both earlier operations are kept, each under its own archive key.
	if len(db.archived) != 2 || value(db.archived[p.ArchiveKey()], "PlanID") != p.ID || value(db.archived[next.ArchiveKey()], "PlanID") != next.ID {
		t.Fatal("an archived operation was lost or replaced")
	}
	for _, old := range []provision.Plan{p, next} {
		if archived, err := d.Archived(ctx, old); err != nil || archived.Plan.ID != old.ID {
			t.Fatal("archived operation unreadable", err)
		}
		if _, err := d.Supersede(ctx, old, "late"); err == nil {
			t.Fatal("an archived plan took the name back")
		}
	}
}
