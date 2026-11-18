package managed

import (
	"context"
	"errors"
	"slices"
	"time"
)

const LegacyTestnetV5 = "legacy-testnet-v5"

// WithMigration binds the narrowly supported runtime migration into the plan
// confirmation. Ordinary image-only plans retain their strict preservation rules.
func (p *Plan) WithMigration(m Migration) error {
	p.Migration = &m
	p.ID = ""
	p.ID = hash(*p)
	return p.Validate()
}

func (p Plan) validateMigration() error {
	if p.Migration == nil {
		return nil
	}
	m := p.Migration
	if m.FleetMiningPause {
		if !m.ReadinessDriven || len(m.Miners) == 0 || len(m.Canaries) != 2 || m.Canaries[0] == m.Canaries[1] {
			return errors.New("fleet pause requires explicit miners, readiness and two architecture canaries")
		}
		arches := map[string]bool{}
		for _, name := range m.Canaries {
			for _, t := range p.Snapshot.Fleet.Targets {
				if t.Name == name && p.Selects(t) {
					arches[t.Architecture] = true
				}
			}
		}
		if !arches["amd64"] || !arches["arm64"] {
			return errors.New("select one amd64 and one arm64 canary")
		}
	} else if len(m.Canaries) > 0 {
		return errors.New("canaries require fleet pause policy")
	}
	if m.Kind != LegacyTestnetV5 || m.EpochStart.IsZero() || m.EpochStart.After(p.CreatedAt) || p.CreatedAt.Sub(m.EpochStart) > 24*time.Hour || p.Operation != "upgrade" || p.Snapshot.Fleet.ChainType != "testnet" {
		return errors.New("legacy-testnet-v5 requires a testnet upgrade and a recent chain-verified epoch start")
	}
	if len(m.Miners) > 0 && !m.ReadinessDriven {
		return errors.New("mining pauses require readiness-driven checks")
	}
	seenMiners := map[string]bool{}
	for _, name := range m.Miners {
		if seenMiners[name] {
			return errors.New("duplicate mining target")
		}
		seenMiners[name] = true
		found := false
		for _, t := range p.Snapshot.Fleet.Targets {
			if t.Name == name && t.Role == "core" && t.MiningService == "dashd-generate-miner.service" && !p.Selects(t) {
				if err := p.Snapshot.CompleteTargets([]string{name}); err != nil {
					return err
				}
				o := p.Snapshot.Nodes[name]
				if o.Mining == nil || !o.Mining.Active || !digestRE.MatchString(o.Mining.ServiceHash) {
					return errors.New("active mining service baseline required")
				}
				found = true
			}
		}
		if !found {
			return errors.New("miners must be explicit unselected Core-only fleet targets")
		}
	}
	if len(m.Miners) > 4 {
		return errors.New("at most four explicit managed miners")
	}
	for _, t := range p.Snapshot.Fleet.Targets {
		if !p.Selects(t) {
			continue
		}
		if t.Role != "validator" {
			return errors.New("v5 migration selects validators only")
		}
		pins := p.Images[t.Name]
		if len(pins) != 4 {
			return errors.New("v5 migration requires exactly core,drive,tenderdash,dapi; helper and companions stay unchanged")
		}
		for _, c := range []string{"core", "drive", "tenderdash", "dapi"} {
			if pins[c] == "" {
				return errors.New("v5 migration requires a coordinated Core and Platform restart")
			}
		}
		before := p.Snapshot.Nodes[t.Name]
		for _, component := range []string{"drive", "dapi"} {
			if slices.Contains(before.Components[component].Digests, pins[component]) {
				return errors.New("v5 migration requires explicit new Drive and DAPI release artifacts")
			}
		}
		if !slices.Contains(before.Components["core"].Digests, pins["core"]) {
			return errors.New("v5 migration must preserve each node's exact captured Core digest")
		}
		if before.Chain.PlatformProtocol != 13 && before.Chain.PlatformProtocol != 14 {
			return errors.New("v5 migration supports protocol 13 to 14 only")
		}
	}
	return nil
}

// The host proves the complete file set is unchanged after reversing ONLY the
// fixed migration delta. A receipt from a different operation is not authority.
func (p Plan) normalized(o Observation) Observation {
	if p.Migration != nil && o.Migration != nil && o.Migration.OperationID == p.ID && o.Migration.After == o.FilesHash && digestRE.MatchString(o.Migration.Before) {
		o.FilesHash = o.Migration.Before
	}
	return o
}

func samePlannedIdentity(base, first, current Observation, t Target, p *Plan) bool {
	if p == nil || p.Migration == nil {
		return SameIdentity(base, current, t)
	}
	current = p.normalized(current)
	if t.Role == "validator" {
		a, b := first.Chain.PlatformProtocol, current.Chain.PlatformProtocol
		if (a != 13 && a != 14) || (b != 13 && b != 14) || b < a || a < base.Chain.PlatformProtocol {
			return false
		}
		// A protocol activation is network-wide, including unselected observers.
		base.Chain.PlatformProtocol = b
	}
	return SameIdentity(base, current, t)
}

func (r Runner) planHealth(ctx context.Context, p Plan) (Health, error) {
	if p.Rollout != nil || p.Migration != nil && p.Migration.ReadinessDriven {
		return r.readinessHealth(ctx, p)
	}
	return doctorWithPlan(ctx, p.Snapshot, r.Remote, r.window(), r.wait, p.Targets, &p)
}
