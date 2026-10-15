package managed

import (
	"context"
	"testing"
	"time"
)

func migrationFixture(t *testing.T) (Plan, Runner, *backend) {
	s, r, _, b := setup(t)
	s.Fleet.ChainType, s.Fleet.CoreNetwork = "testnet", "test"
	for n, o := range s.Nodes {
		o.Chain.CoreNetwork = "test"
		o.Chain.PlatformProtocol = 13
		s.Nodes[n] = o
		b.nodes[n] = o
	}
	s.ID = ""
	s.ID = hash(s)
	r.Cloud = &cloud{f: s.Fleet}
	images := pins(s, "core,drive,tenderdash,dapi", "b")
	for n := range images {
		images[n]["core"] = s.Nodes[n].Components["core"].Digests[0]
	}
	p, e := Build(s, "upgrade", "core,drive,tenderdash,dapi", "", images, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if e = p.WithMigration(Migration{Kind: LegacyTestnetV5, EpochStart: time.Now().Add(-10 * time.Minute)}); e != nil {
		t.Fatal(e)
	}
	return p, r, b
}

func TestMigrationBindsScopeCoreAndEpoch(t *testing.T) {
	p, _, _ := migrationFixture(t)
	cases := map[string]func(*Plan){
		"core downgrade": func(p *Plan) {
			p.Images[p.Snapshot.Fleet.Targets[0].Name]["core"] = p.Images[p.Snapshot.Fleet.Targets[0].Name]["drive"]
		},
		"platform only":     func(p *Plan) { p.Scope = "platform" },
		"missing epoch":     func(p *Plan) { p.Migration.EpochStart = time.Time{} },
		"future epoch":      func(p *Plan) { p.Migration.EpochStart = p.CreatedAt.Add(time.Hour) },
		"old epoch":         func(p *Plan) { p.Migration.EpochStart = p.CreatedAt.Add(-25 * time.Hour) },
		"unknown migration": func(p *Plan) { p.Migration.Kind = "other" },
		"deploy":            func(p *Plan) { p.Operation = "deploy" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			q := clone(p)
			change(&q)
			q.ID = ""
			q.ID = hash(q)
			if q.Validate() == nil {
				t.Fatal("unsafe migration accepted")
			}
		})
	}
	q := clone(p)
	q.Migration.EpochStart = q.Migration.EpochStart.Add(time.Second)
	if q.Validate() == nil {
		t.Fatal("policy change did not invalidate confirmation")
	}
}

func TestMigrationIdentityReceiptIsNarrowAndOperationBound(t *testing.T) {
	p, _, _ := migrationFixture(t)
	target := p.Snapshot.Fleet.Targets[0]
	base := p.Snapshot.Nodes[target.Name]
	current := clone(base)
	current.FilesHash = h64("migrated")
	current.Chain.PlatformProtocol = 14
	current.Migration = &MigrationReceipt{OperationID: p.ID, Before: base.FilesHash, After: current.FilesHash}
	if !samePlannedIdentity(base, base, current, target, &p) {
		t.Fatal("expected migration rejected")
	}
	if SameIdentity(base, current, target) {
		t.Fatal("ordinary doctor weakened")
	}
	cases := map[string]func(*Observation){
		"unrelated files": func(o *Observation) { o.FilesHash = h64("other") },
		"wrong operation": func(o *Observation) { o.Migration.OperationID = h64("other") },
		"wrong baseline":  func(o *Observation) { o.Migration.Before = h64("other") },
		"identity":        func(o *Observation) { o.Chain.ProTxHash = h64("other") },
		"genesis":         func(o *Observation) { o.Chain.CoreGenesis = h64("other") },
		"protocol15":      func(o *Observation) { o.Chain.PlatformProtocol = 15 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			o := clone(current)
			change(&o)
			if samePlannedIdentity(base, base, o, target, &p) {
				t.Fatal("drift accepted")
			}
		})
	}
	first := clone(current)
	current.Chain.PlatformProtocol = 13
	if samePlannedIdentity(base, first, current, target, &p) {
		t.Fatal("protocol regression accepted")
	}
}

func TestMigrationRunnerSurvivesProtocolActivationAndReceipt(t *testing.T) {
	p, r, b := migrationFixture(t)
	enroll(t, p.Snapshot, r)
	b.after = func(q Request) error {
		if q.Action == "apply" {
			if q.Migration == nil {
				t.Fatal("migration lost in transport")
			}
			o := b.nodes[q.Target.Name]
			o.FilesHash = h64(q.Target.Name + "newfiles")
			o.Migration = &MigrationReceipt{OperationID: p.ID, Before: q.Expected.FilesHash, After: o.FilesHash}
			b.nodes[q.Target.Name] = o
			// Activation changes the whole network, not only the node being upgraded.
			for n, v := range b.nodes {
				v.Chain.PlatformProtocol = 14
				b.nodes[n] = v
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	record, e := r.Execute(ctx, p)
	if e != nil || record.Phase != "complete" {
		t.Fatalf("migration failed: %v %s", e, record.Phase)
	}
}
