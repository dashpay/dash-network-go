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
	if m.Kind != LegacyTestnetV5 || m.EpochStart.IsZero() || m.EpochStart.After(p.CreatedAt) || p.CreatedAt.Sub(m.EpochStart) > 24*time.Hour || p.Operation != "upgrade" || p.Snapshot.Fleet.ChainType != "testnet" {
		return errors.New("legacy-testnet-v5 requires a testnet upgrade and a recent chain-verified epoch start")
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
		if (a != 13 && a != 14) || (b != 13 && b != 14) || b < a || b < base.Chain.PlatformProtocol {
			return false
		}
		// A protocol activation is network-wide, including unselected observers.
		base.Chain.PlatformProtocol = b
	}
	return SameIdentity(base, current, t)
}

func (r Runner) planHealth(ctx context.Context, p Plan) (Health, error) {
	return doctorWithPlan(ctx, p.Snapshot, r.Remote, r.window(), r.wait, p.Targets, &p)
}
