package plan_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dashpay/dash-network-go/internal/plan"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

func TestPlatformPlanPreservesCoreAndExcludesWalletMiner(t *testing.T) {
	n := testutil.Network(t)
	l := testutil.Lock(t, n)
	p, err := plan.Build(n, l, "upgrade", "platform")
	if err != nil {
		t.Fatal(err)
	}
	if p.Executable {
		t.Fatal("foundation advertised executable plan")
	}
	if !slices.Contains(p.Preserve, "core-processes-and-configuration") {
		t.Fatal("missing Core preservation boundary")
	}
	for _, i := range p.Images {
		if i.Component == "core" {
			t.Fatal("platform plan selected Core image")
		}
	}
	for _, g := range p.Targets {
		if g.Role == "wallet" || g.Role == "miner" {
			t.Fatal("platform plan touched wallet/miner")
		}
	}
	if len(p.Targets) != 2 {
		t.Fatal("platform plan lost validator or seed targets")
	}
	l.ResolvedAt = l.ResolvedAt.AddDate(0, 0, 1)
	again, err := plan.Build(n, l, "upgrade", "platform")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatal("same resolved intent did not produce deterministic plan")
	}
}

func TestTestnetCannotBeCreatedAndCoreScopeIsNarrow(t *testing.T) {
	n, err := spec.Load("../../examples/testnet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	l := testutil.Lock(t, n)
	if _, err := plan.Build(n, l, "create", "all"); err == nil {
		t.Fatal("testnet creation accepted")
	}
	p, err := plan.Build(n, l, "upgrade", "core")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Images) != 1 || p.Images[0].Component != "core" {
		t.Fatal("core scope includes unrelated images")
	}
	if _, err := plan.Build(n, l, "upgrade", "platfrom"); err == nil {
		t.Fatal("unknown scope accepted")
	}
}
