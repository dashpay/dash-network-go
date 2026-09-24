package spec_test

import (
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/dashpay/dash-network-go/internal/testutil"
	"testing"
)

func TestProvisionBoundary(t *testing.T) {
	cases := map[string]func(*spec.Network){
		"testnet": func(n *spec.Network) { n.Chain.Type = "testnet"; n.Metadata.Name = "testnet" },
		"testnet-namespace": func(n *spec.Network) {
			n.Chain.Type = "testnet"
			n.Metadata.Name = "testnet"
			for i := range n.Nodes {
				n.Nodes[i].Role = "fullnode"
			}
		},
		"testnet-validator": func(n *spec.Network) { n.Chain.Type = "testnet"; n.Metadata.Name = "testnet-extra" },
		"missing":           func(n *spec.Network) { n.AWS.Provision = nil },
		"reserved-tag":      func(n *spec.Network) { n.AWS.NetworkTagKey = "dashnet:node" },
		"name-tag":          func(n *spec.Network) { n.AWS.NetworkTagKey = "Name" },
		"aws-tag":           func(n *spec.Network) { n.AWS.NetworkTagKey = "AWS:reserved" },
		"wildcard-tag":      func(n *spec.Network) { n.AWS.NetworkTagKey = "Network?" },
		"no-state":          func(n *spec.Network) { n.AWS.Provision.StateTable = "" },
		"bad-subnet":        func(n *spec.Network) { n.AWS.Provision.SubnetID = "vpc-00000001" },
		"duplicate-sg":      func(n *spec.Network) { n.AWS.Provision.SecurityGroupIDs = []string{"sg-00000001", "sg-00000001"} },
		"implicit-sg":       func(n *spec.Network) { n.AWS.Provision.SecurityGroupIDs = nil },
		"no-emergency-key":  func(n *spec.Network) { n.AWS.Provision.KeyName = "" },
		"implicit-owner":    func(n *spec.Network) { n.AWS.Provision.AMIs["arm64"] = spec.AMI{ID: "ami-00000001"} },
		"disk":              func(n *spec.Network) { n.AWS.Provision.RootVolumeGiB = 0 },
		"oversized":         func(n *spec.Network) { n.Nodes[0].Count = 101 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			n := testutil.ProvisionNetwork(t)
			change(&n)
			if err := n.ValidateProvision(); err == nil {
				t.Fatal("unsafe provisioning config accepted")
			}
		})
	}
	n := testutil.ProvisionNetwork(t)
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
}
