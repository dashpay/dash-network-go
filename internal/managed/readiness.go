package managed

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"
)

const readinessPoll = 15 * time.Second
const progressTimeout = 10 * time.Minute

func referenceHeight(s Snapshot) int64 {
	var n int64
	for _, t := range s.Fleet.Targets {
		if t.Role == "validator" {
			h := s.Nodes[t.Name].Chain.PlatformHeight
			if h > 0 && (n == 0 || h < n) {
				n = h
			}
		}
	}
	return n
}

// agreementHealth is the batch gate that asks for no new block: one sample for
// health and identity, then one that reads every validator's block at a height
// they have all reached. A host reports a Platform block hash only for such a
// reference height, so a single unreferenced sample can never show agreement.
func (r Runner) agreementHealth(ctx context.Context, baseline Snapshot, p Plan, label string) Health {
	first := Observe(ctx, baseline.Fleet, r.Remote, 0)
	second := first
	if reference := referenceHeight(first); reference > 0 {
		second = Observe(ctx, baseline.Fleet, r.Remote, reference)
	}
	return assessHealth(baseline, first, second, &p, label, false)
}

// Poll from a fixed baseline, not a succession of short windows that each
// demand their own block. A slow block never becomes an arbitrary sleep floor.
func (r Runner) readinessHealth(ctx context.Context, p Plan) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, progressTimeout)
	defer cancel()
	baseline := p.Snapshot
	if p.Rollout != nil {
		baseline = r.rolloutSnapshot(p)
	}
	if p.Rollout != nil && len(r.batchTargets) > 0 {
		return r.agreementHealth(ctx, baseline, p, "target readiness"), ctx.Err()
	}
	r.report("managed health gate waiting: sampling selected targets for readiness and chain progress")
	first := Observe(ctx, baseline.Fleet, r.Remote, 0)
	first.Nodes = maps.Clone(first.Nodes)
	previous := first
	reference := referenceHeight(first)
	targets := map[string]Target{}
	for _, t := range baseline.Fleet.Targets {
		targets[t.Name] = t
	}
	for {
		second := Observe(ctx, baseline.Fleet, r.Remote, reference)
		// Detect regressions between polls even if still above the original sample.
		continuity := assessHealth(baseline, previous, second, &p, "readiness", false)
		for _, problem := range continuity.Problems {
			// A host that could not be observed this round proves nothing about
			// the chain: its empty answer is not a fork or a stall. It stays
			// unhealthy and is sampled again; only an observed regression ends
			// the wait.
			if name, _, found := strings.Cut(problem, ": "); found {
				if t, known := targets[name]; known && second.Nodes[name].Healthy(t) != nil {
					continue
				}
			}
			if strings.Contains(problem, "identity/configuration/protocol changed") || strings.Contains(problem, "consensus block disagreement") || strings.Contains(problem, "not advancing") || strings.Contains(problem, "PoSe penalty increased") {
				return continuity, errors.New("readiness continuity failed: " + problem)
			}
		}
		h := assessHealthProgress(baseline, first, second, &p, "readiness", true, !r.fleetPaused)
		if r.rolloutHealthy(h, p) {
			r.report("managed health gate passed: readiness and required chain progress verified")
			return h, nil
		}
		// Permanent faults do not become healthy merely because time passed.
		for _, t := range p.Snapshot.Fleet.Targets {
			if p.Selects(t) && (second.Nodes[t.Name].Chain.MasternodeState == "POSE_BANNED" || second.Nodes[t.Name].Chain.MasternodeState == "REMOVED") {
				return h, errors.New("masternode banned or removed")
			}
		}
		// A baseline that failed to observe a host carries that failure into
		// every later round and could never pass: start that host's baseline
		// from its first clean observation instead.
		for name, t := range targets {
			if first.Nodes[name].Healthy(t) != nil && second.Nodes[name].Healthy(t) == nil {
				first.Nodes[name] = second.Nodes[name]
			}
		}
		if reference == 0 {
			reference = referenceHeight(second)
		}
		previous = second
		if err := r.wait(ctx, readinessPoll); err != nil {
			return h, err
		}
	}
}

// A fresh pre-withdrawal safety sample need not demand yet another new block
// after a successful post-withdrawal progression proof. It still checks actual
// health, identities, common Platform history and current available quorum.
func (r Runner) withdrawalHealth(ctx context.Context, p Plan) (Health, error) {
	if p.Rollout == nil && (p.Migration == nil || !p.Migration.ReadinessDriven) {
		return r.planHealth(ctx, p)
	}
	if p.Rollout != nil {
		return r.agreementHealth(ctx, r.rolloutSnapshot(p), p, "batch readiness"), ctx.Err()
	}
	first := Observe(ctx, p.Snapshot.Fleet, r.Remote, 0)
	second := Observe(ctx, p.Snapshot.Fleet, r.Remote, referenceHeight(first))
	return assessHealth(p.Snapshot, first, second, &p, "fresh withdrawal", false), ctx.Err()
}
