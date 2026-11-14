package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
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

// archiveKey is where a plan's operation is kept once the given item, its live
// record, has been replaced.
func archiveKey(t *testing.T, p provision.Plan, item map[string]types.AttributeValue) string {
	t.Helper()
	n, ok := item["Revision"].(*types.AttributeValueMemberN)
	if !ok {
		t.Fatal("journal item has no revision")
	}
	revision, err := strconv.ParseInt(n.Value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return p.ArchiveKey(revision)
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
	archive := archiveKey(t, p, before)
	kept := db.archived[archive]
	if len(db.archived) != 1 || len(kept) != 4 || r.Predecessor.Archive != archive || value(kept, "Network") != archive || value(kept, "PlanID") != p.ID || value(kept, "Data") != value(before, "Data") || !sameNumber(kept["Revision"], before["Revision"]) {
		t.Fatal("retired operation was not archived verbatim")
	}
	if value(db.item, "Network") != p.Key() || value(db.item, "PlanID") != next.ID || value(db.item, "Owner") != "one" || !sameNumber(db.item["Revision"], number(0)) {
		t.Fatal("live item is not the successor's claimed operation")
	}
	current, owner, err := d.Read(ctx, next)
	if err != nil || owner != "one" || current.Predecessor.Archive != archive || current.Predecessor.Revision != r.Predecessor.Revision {
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
	if value(db.archived[archive], "Data") != value(before, "Data") {
		t.Fatal("archive changed after the handover")
	}
}

// A retired operation is the proof that releases its network's name. No stage
// may claim it and write new checkpoints into it: a retried deployment used to.
func TestRetiredOperationIsNeverClaimedOrChangedAgain(t *testing.T) {
	d, db, p, next, _ := retiredFixture(t)
	ctx := context.Background()
	// A record today's rules accept in full, so only its retirement stands in the way.
	rewrite(t, db, func(fields map[string]json.RawMessage) {
		for _, field := range historyFields {
			delete(fields, field)
		}
	})
	if r, _, err := d.Read(ctx, p); err != nil || r.Phase != "addresses-released" {
		t.Fatal("fixture is not a fully valid retired operation", err)
	}
	before, saves := clone(db.item), db.saves
	untouched := func() bool {
		return len(db.item) == len(before) && value(db.item, "Owner") == "" && value(db.item, "Data") == value(before, "Data") && sameNumber(db.item["Revision"], before["Revision"]) && db.saves == saves
	}
	if _, err := d.Acquire(ctx, p, "deploy"); !errors.Is(err, provision.ErrRetired) || !untouched() {
		t.Fatal("a stage claimed a retired operation", err)
	}
	// Retired between the stage's read and its claim: the claim is given back unused.
	retiredData := value(db.item, "Data")
	rewrite(t, db, func(fields map[string]json.RawMessage) { fields["phase"] = json.RawMessage(`"compute-ready"`) })
	db.claimResponse = func(out *dynamodb.UpdateItemOutput) (*dynamodb.UpdateItemOutput, error) {
		db.item["Data"] = str(retiredData)
		out.Attributes["Data"] = str(retiredData)
		return out, nil
	}
	if _, err := d.Acquire(ctx, p, "deploy"); !errors.Is(err, provision.ErrRetired) || !untouched() {
		t.Fatal("a stage kept a claim on an operation retired under it", err)
	}
	db.claimResponse = nil
	// Address cleanup may still look, and changes nothing. The name is then handed over.
	cleanup := d.AddressCleanup()
	if r, err := cleanup.Acquire(ctx, p, "cleanup"); err != nil || r.Phase != "addresses-released" {
		t.Fatal("address cleanup can no longer replay a retired operation", err)
	}
	if err := cleanup.Release(ctx, p, "cleanup"); err != nil || !untouched() {
		t.Fatal("address cleanup changed a retired operation", err)
	}
	r, err := d.Supersede(ctx, next, "runner")
	if err != nil || r.Predecessor.Archive != archiveKey(t, p, before) || value(db.archived[r.Predecessor.Archive], "Data") != value(before, "Data") {
		t.Fatal("handover failed after refused claims", err)
	}
	// The successor, once retired in its turn, is as closed as its predecessor.
	for _, target := range next.Targets {
		at := r.CreatedAt
		r.Nodes[target.Name] = provision.NodeProgress{Phase: "present", AttemptedAt: at, ObservedAt: at, EC2State: "terminated", InstanceID: "i-0" + target.Name}
	}
	r.Phase = "addresses-released"
	r.Revision++
	if err = d.Save(ctx, r, "runner"); err != nil {
		t.Fatal(err)
	}
	if err = d.Release(ctx, next, "runner"); err != nil {
		t.Fatal(err)
	}
	for _, acquire := range []func(context.Context, provision.Plan, string) (provision.Record, error){d.Acquire, d.Supersede} {
		if _, err = acquire(ctx, next, "late"); !errors.Is(err, provision.ErrRetired) || value(db.item, "Owner") != "" {
			t.Fatal("a retired successor was claimed again", err)
		}
	}
}

// The sequence that blocked a name for good: the handover archived the retired
// operation and stopped; the operation then changed (two more checkpoints, from
// a release that still let a stage claim it); every later handover found an
// archive that no longer matched and refused.
func TestHandoverIsNotBlockedByAnArchiveOfAnEarlierRevision(t *testing.T) {
	d, db, p, next, _ := retiredFixture(t)
	ctx := context.Background()
	first := archiveKey(t, p, db.item)
	original := value(db.item, "Data")
	db.replace = "refuse"
	if _, err := d.Supersede(ctx, next, "first"); err == nil || len(db.archived) != 1 || value(db.archived[first], "Data") != original || value(db.item, "PlanID") != p.ID {
		t.Fatal("handover did not stop after the archive", err)
	}
	db.replace = ""
	var revision int64
	rewrite(t, db, func(fields map[string]json.RawMessage) {
		if err := json.Unmarshal(fields["revision"], &revision); err != nil {
			t.Fatal(err)
		}
		revision += 2
		fields["revision"], _ = json.Marshal(revision)
		fields["lastError"] = json.RawMessage(`"deployment retried against a retired network"`)
	})
	db.item["Revision"] = number(revision)
	second, changed := archiveKey(t, p, db.item), value(db.item, "Data")
	if second == first || changed == original {
		t.Fatal("fixture did not change the retired operation")
	}
	if succeeds, reused, err := d.Succession(ctx, p); err != nil || succeeds != p.ID || !reused {
		t.Fatal("changed retired operation is no longer plannable", err)
	}
	r, err := d.Supersede(ctx, next, "second")
	if err != nil || db.replaced != 1 || value(db.item, "PlanID") != next.ID || value(db.item, "Owner") != "second" || r.Predecessor.Archive != second || r.Predecessor.Revision != revision {
		t.Fatal("handover blocked by an archive of an earlier revision", err)
	}
	// Both copies are kept: the one that was replaced, and the earlier one.
	if len(db.archived) != 2 || value(db.archived[second], "Data") != changed || value(db.archived[first], "Data") != original {
		t.Fatal("an archived revision was lost or overwritten")
	}
	archived, err := d.Archived(ctx, p)
	if err != nil || archived.Revision != revision || archived.LastError == "" {
		t.Fatal("the archived operation is not the revision that was replaced", err)
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
	archive := archiveKey(t, p, db.item)
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
	if wins != 1 || db.replaced != 1 || value(db.item, "PlanID") != winner.ID || value(db.item, "Owner") == "" || len(db.archived) != 1 || value(db.archived[archive], "PlanID") != p.ID {
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
		before, archive := value(db.item, "Data"), archiveKey(t, p, db.item)
		db.replace = "refuse"
		if _, err := d.Supersede(ctx, next, "first"); err == nil || db.replaced != 0 || value(db.item, "PlanID") != p.ID || value(db.item, "Owner") != "" || len(db.archived) != 1 {
			t.Fatal("interrupted handover left an unexpected journal", err)
		}
		// Still retired, still plannable, and the archive is not written twice.
		if succeeds, reused, err := d.Succession(ctx, p); err != nil || succeeds != p.ID || !reused {
			t.Fatal("retired operation lost after an interrupted handover", err)
		}
		db.replace = ""
		if _, err := d.Supersede(ctx, next, "second"); err != nil || db.replaced != 1 || len(db.archived) != 1 || value(db.archived[archive], "Data") != before || value(db.item, "Owner") != "second" {
			t.Fatal("repeat after an interrupted handover failed", err)
		}
	})
	t.Run("archive-of-this-revision-holds-something-else", func(t *testing.T) {
		// Same plan, same revision, other data: no checkpoint of this tool does
		// that. Nothing is overwritten and nothing is handed over.
		d, db, p, next, _ := retiredFixture(t)
		archive := archiveKey(t, p, db.item)
		db.archived = map[string]map[string]types.AttributeValue{archive: {"Network": str(archive), "PlanID": str(p.ID), "Data": str("{}"), "Revision": db.item["Revision"]}}
		if _, err := d.Supersede(context.Background(), next, "runner"); err == nil || db.replaced != 0 || value(db.item, "PlanID") != p.ID || value(db.archived[archive], "Data") != "{}" {
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
	first := r.Predecessor.Archive
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
	if len(db.archived) != 2 || fresh.Predecessor.Archive == first || value(db.archived[first], "PlanID") != p.ID || value(db.archived[fresh.Predecessor.Archive], "PlanID") != next.ID {
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
