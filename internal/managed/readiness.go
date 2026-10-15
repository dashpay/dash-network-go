package managed

import (
	"context"
	"errors"
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

// Poll from a fixed baseline, not a succession of short windows that each
// demand their own block. A slow block never becomes an arbitrary sleep floor.
func (r Runner) readinessHealth(ctx context.Context, p Plan) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, progressTimeout)
	defer cancel()
	first := Observe(ctx, p.Snapshot.Fleet, r.Remote, 0)
	previous := first
	reference := referenceHeight(first)
	for {
		second := Observe(ctx, p.Snapshot.Fleet, r.Remote, reference)
		// Detect regressions between polls even if still above the original sample.
		continuity := assessHealth(p.Snapshot, previous, second, &p, "readiness", false)
		for _, problem := range continuity.Problems {
			if strings.Contains(problem, "identity/configuration/protocol changed") || strings.Contains(problem, "consensus block disagreement") || strings.Contains(problem, "not advancing") || strings.Contains(problem, "PoSe penalty increased") {
				return continuity, errors.New("readiness continuity failed: " + problem)
			}
		}
		h := assessHealth(p.Snapshot, first, second, &p, "readiness", true)
		if healthSelected(h, p) {
			return h, nil
		}
		// Permanent faults do not become healthy merely because time passed.
		for _, t := range p.Snapshot.Fleet.Targets {
			if p.Selects(t) && (second.Nodes[t.Name].Chain.MasternodeState == "POSE_BANNED" || second.Nodes[t.Name].Chain.MasternodeState == "REMOVED") {
				return h, errors.New("masternode banned or removed")
			}
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
	if p.Migration == nil || !p.Migration.ReadinessDriven {
		return r.planHealth(ctx, p)
	}
	first := Observe(ctx, p.Snapshot.Fleet, r.Remote, 0)
	second := Observe(ctx, p.Snapshot.Fleet, r.Remote, referenceHeight(first))
	return assessHealth(p.Snapshot, first, second, &p, "fresh withdrawal", false), ctx.Err()
}
