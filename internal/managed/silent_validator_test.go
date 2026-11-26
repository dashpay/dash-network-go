package managed

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dashpay/dash-network-go/internal/rollout"
)

// platformDown is what the host worker returns for a validator whose Platform
// cannot be read: Core answers, and no Platform field is present at all.
func platformDown(o *Observation) {
	c := o.Chain
	o.Problems = []string{"platform-health-unavailable"}
	o.Chain = Chain{PosePenalty: c.PosePenalty, CoreNetwork: c.CoreNetwork, CoreGenesis: c.CoreGenesis, CoreCheckpointHash: c.CoreCheckpointHash, CoreHash: c.CoreHash, CoreHeight: c.CoreHeight, CoreSynced: c.CoreSynced, ChainLockHeight: c.ChainLockHeight, MasternodeState: c.MasternodeState, ProTxHash: c.ProTxHash}
}

// oneValidatorRollout is the shape that failed on Testnet: a reviewed Core
// upgrade of one validator out of 29, with the owned miners paused.
func oneValidatorRollout(t *testing.T, chosen string) (Plan, Runner, *fleetBackend) {
	t.Helper()
	p, r, b := fleetFixture(t)
	images := Images{}
	for _, target := range p.Snapshot.Fleet.Targets {
		images[target.Name] = map[string]string{}
	}
	images[chosen]["core"] = "docker.io/dashpay/core@sha256:" + strings.Repeat("c", 64)
	p, err := BuildSelected(p.Snapshot, "upgrade", "core", "", images, []string{chosen}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = p.WithRollout(rollout.Options{Concurrency: 1, PauseMining: true, Miners: []string{"miner-1", "miner-2"}}); err != nil {
		t.Fatal(err)
	}
	// A gate that cannot pass must end the test, not spin until its deadline.
	var waits atomic.Int32
	r.Wait = func(ctx context.Context, _ time.Duration) error {
		if waits.Add(1) > 200 {
			return context.DeadlineExceeded
		}
		return ctx.Err()
	}
	return p, r, b
}

func applied(b *fleetBackend) (names []string) {
	for _, q := range b.calls {
		if q.Action == "apply" {
			names = append(names, q.Target.Name)
		}
	}
	return
}

func TestRolloutPassesAValidatorThatIsOffTheChain(t *testing.T) {
	for _, fault := range []string{"platform down", "unreachable"} {
		for _, pair := range [][2]string{
			{"validator-25", "validator-29"}, // neither is in the active quorum
			{"validator-01", "validator-02"}, // both are: 10 of 12 members remain
		} {
			t.Run(fault+" "+pair[0], func(t *testing.T) {
				chosen, silent := pair[0], pair[1]
				p, r, b := oneValidatorRollout(t, chosen)
				if fault == "platform down" {
					previous := b.base.observe
					b.base.observe = func(q Request, o *Observation) {
						previous(q, o)
						if q.Target.Name == silent {
							platformDown(o)
						}
					}
				} else {
					r.Remote = stagingBackend(func(ctx context.Context, q Request) (Observation, error) {
						if q.Target.Name == silent {
							return Observation{}, errors.New("ssh: connection timed out")
						}
						return b.Call(ctx, q)
					})
				}
				ctx, c := deadline()
				defer c()
				record, err := r.Execute(ctx, p)
				if err != nil || record.Phase != "complete" || record.MiningPhase != "resumed" || len(b.paused) != 0 {
					t.Fatal(err, record.Phase, record.MiningPhase, b.paused)
				}
				if got := applied(b); !slices.Equal(got, []string{chosen}) {
					t.Fatal("applied", got)
				}
				if image := b.base.nodes[chosen].Components["core"].Image; image != p.Images[chosen]["core"] {
					t.Fatal("selected Core not replaced", image)
				}
			})
		}
	}
}

// The validator that is off the chain is still lost voting power.
func TestRolloutCountsValidatorsThatAreOffTheChainAsLostVotingPower(t *testing.T) {
	chosen := "validator-01"
	silent := []string{"validator-02", "validator-03", "validator-04", "validator-05"}
	p, r, b := oneValidatorRollout(t, chosen)
	previous := b.base.observe
	b.base.observe = func(q Request, o *Observation) {
		previous(q, o)
		if slices.Contains(silent, q.Target.Name) {
			platformDown(o)
		}
	}
	ctx, c := deadline()
	defer c()
	// Seven of the twelve members would remain: not more than two thirds.
	record, err := r.Execute(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "lacks observed >2/3 voting power") {
		t.Fatal(err)
	}
	if got := applied(b); len(got) != 0 || len(b.paused) != 0 || record.MiningPhase != "resumed" {
		t.Fatal(got, b.paused, record.MiningPhase)
	}
}

// Two different blocks at one height are a fork, whoever reports them.
func TestRolloutStopsWhenAnUnselectedValidatorReportsAnotherBlock(t *testing.T) {
	p, r, b := oneValidatorRollout(t, "validator-25")
	previous := b.base.observe
	b.base.observe = func(q Request, o *Observation) {
		previous(q, o)
		if q.Target.Name == "validator-29" && q.ReferenceHeight > 0 {
			o.Chain.PlatformHash = h64("another-block")
		}
	}
	ctx, c := deadline()
	defer c()
	_, err := r.Execute(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "consensus block disagreement") {
		t.Fatal(err)
	}
	for _, q := range b.calls {
		if q.Action == "apply" || q.Action == "stage" || q.Action == "mining_pause" {
			t.Fatal("changed a host after a fork was observed", q.Action, q.Target.Name)
		}
	}
}

func TestHealthKeepsAMissingBlockApartFromADifferentOne(t *testing.T) {
	s, _, _, b := setup(t)
	sample := func(change func(name string, o *Observation)) Health {
		b.observe = nil
		first := Observe(context.Background(), s.Fleet, b, 0)
		b.observe = func(q Request, o *Observation) { change(q.Target.Name, o) }
		return assessHealth(s, first, Observe(context.Background(), s.Fleet, b, referenceHeight(first)), nil, "test", true)
	}
	h := sample(func(name string, o *Observation) {
		if name == "validator-07" {
			platformDown(o)
		}
	})
	for name, n := range h.Nodes {
		if n.Healthy == (name == "validator-07") {
			t.Fatal(name, n.Problems)
		}
	}
	if !slices.Contains(h.Nodes["validator-07"].Problems, "validator-07: Platform block unavailable") || h.Healthy {
		t.Fatal(h.Nodes["validator-07"].Problems, h.Healthy)
	}
	// A real fork: nobody can tell which side is right, so nobody is healthy.
	s, _, _, b = setup(t)
	h = sample(func(name string, o *Observation) {
		if name == "validator-07" {
			o.Chain.PlatformHash = h64("another-block")
		}
	})
	for name, n := range h.Nodes {
		if n.Healthy || !slices.Contains(n.Problems, "consensus block disagreement") {
			t.Fatal(name, n.Problems)
		}
	}
}

func TestWithdrawalNeedsAQuorumReportedByAHealthyValidator(t *testing.T) {
	s, _, _, _ := setup(t)
	target := s.Fleet.Targets[0]
	alter := func(change func(name string, o *Observation)) Snapshot {
		out := s
		out.Nodes = map[string]Observation{}
		for name, o := range s.Nodes {
			o = clone(o)
			change(name, &o)
			out.Nodes[name] = o
		}
		return out
	}
	// validator-13 is not a member of the quorum the others report.
	if err := CanWithdraw(alter(func(name string, o *Observation) {
		if name == "validator-13" {
			platformDown(o)
		}
	}), target); err != nil {
		t.Fatal(err)
	}
	if err := CanWithdraw(alter(func(name string, o *Observation) {
		if name == "validator-13" {
			*o = Observation{InstanceID: o.InstanceID, At: o.At, Error: "ssh: connection timed out"}
		}
	}), target); err != nil {
		t.Fatal(err)
	}
	// A validator that looks healthy and reports no quorum is not understood.
	if err := CanWithdraw(alter(func(name string, o *Observation) {
		if name == "validator-13" {
			o.Chain.VotingPower = nil
		}
	}), target); err == nil || !strings.Contains(err.Error(), "quorum membership unavailable") {
		t.Fatal(err)
	}
	// Nobody reports a quorum: nothing is known, so nothing is withdrawn.
	if err := CanWithdraw(alter(func(_ string, o *Observation) { platformDown(o) }), target); err == nil || !strings.Contains(err.Error(), "quorum membership unavailable") {
		t.Fatal(err)
	}
}
