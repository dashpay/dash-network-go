package spec_test

import (
	"os"
	"strings"
	"testing"

	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/testutil"
)

func TestDecodeRejectsAmbiguousIntent(t *testing.T) {
	b, err := os.ReadFile("../../examples/devnet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{
		"unknown field":   string(b) + "\nvisiblity: public\n",
		"extra document":  string(b) + "\n---\n{}\n",
		"duplicate field": string(b) + "\nkind: Network\n",
		"empty":           "",
		"oversized":       strings.Repeat(" ", (1<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := spec.Decode(strings.NewReader(input)); err == nil {
				t.Fatal("accepted ambiguous input")
			}
		})
	}
}

func TestNetworkBoundaries(t *testing.T) {
	for name, mutate := range map[string]func(*spec.Network){
		"mainnet":                  func(n *spec.Network) { n.Chain.Type = "mainnet" },
		"missing account":          func(n *spec.Network) { n.AWS.AccountID = "" },
		"implicit visibility":      func(n *spec.Network) { n.Metadata.Visibility = "" },
		"zero generation":          func(n *spec.Network) { n.Chain.Generation = 0 },
		"no nodes":                 func(n *spec.Network) { n.Nodes = nil },
		"duplicate group":          func(n *spec.Network) { n.Nodes = append(n.Nodes, n.Nodes[0]) },
		"unsupported architecture": func(n *spec.Network) { n.Nodes[0].Architecture = "x86_64" },
		"negative count":           func(n *spec.Network) { n.Nodes[0].Count = -1 },
		"implicit tag":             func(n *spec.Network) { n.Images["core"] = "dashpay/dashd" },
		"missing component":        func(n *spec.Network) { delete(n.Images, "drive") },
		"unknown component":        func(n *spec.Network) { n.Images["typo"] = "dashpay/drive:latest" },
	} {
		t.Run(name, func(t *testing.T) {
			n := testutil.Network(t)
			mutate(&n)
			if n.Validate() == nil {
				t.Fatal("accepted invalid intent")
			}
		})
	}
}

func TestFingerprintAndArchitectureSet(t *testing.T) {
	n := testutil.Network(t)
	before := n.Fingerprint()
	n.Chain.Generation++
	if before == n.Fingerprint() {
		t.Fatal("reset did not change identity fingerprint")
	}
	n.Nodes[0].Architecture = "amd64"
	if strings.Join(n.Architectures(), ",") != "amd64,arm64" {
		t.Fatal("architectures not sorted and deduplicated")
	}
	if _, err := spec.Load("../../examples/testnet.yaml"); err != nil {
		t.Fatal(err)
	}
}
