// Package join deploys new owned Core fullnodes onto an existing chain. Compute
// allocations remain separate from legacy fleet manifests; no identity cloning.
package join

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/spec"
)

type Plan struct {
	APIVersion   string         `json:"apiVersion"`
	Kind         string         `json:"kind"`
	ID           string         `json:"id"`
	Bootstrap    bootstrap.Plan `json:"bootstrap"`
	Chain        node.CoreJoin  `json:"chain"`
	RecipeSHA256 string         `json:"recipeSha256"`
	Targets      []node.Target  `json:"targets"`
}

func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func Build(b bootstrap.Plan, j node.CoreJoin, live map[string]types.Instance) (Plan, error) {
	p := Plan{APIVersion: spec.Version, Kind: "CoreJoinPlan", Bootstrap: b, Chain: j, RecipeSHA256: node.JoinRecipeDigest()}
	for i, t := range b.Compute.Targets {
		v, ok := live[t.Name]
		if !ok {
			return p, errors.New("missing join target")
		}
		ip := aws.ToString(v.PrivateIpAddress)
		if b.Access.Address == "public" {
			ip = aws.ToString(v.PublicIpAddress)
		}
		p.Targets = append(p.Targets, node.Target{Name: t.Name, Role: t.Role, Architecture: t.Architecture, InstanceID: aws.ToString(v.InstanceId), SSHAddress: ip, PeerAddress: aws.ToString(v.PrivateIpAddress), Images: b.Targets[i].Images})
	}
	p.ID = hash(p)
	return p, p.Validate()
}
func (p Plan) Validate() error {
	if e := p.Bootstrap.Validate(); e != nil {
		return e
	}
	if e := p.Chain.Validate(); e != nil {
		return e
	}
	copy := p
	copy.ID = ""
	if p.ID != hash(copy) || p.APIVersion != spec.Version || p.Kind != "CoreJoinPlan" || p.RecipeSHA256 != node.JoinRecipeDigest() {
		return errors.New("join plan altered or recipe changed; use retained binary/plan")
	}
	if p.Bootstrap.Compute.Network.Chain.Type != p.Chain.ChainType || len(p.Targets) != len(p.Bootstrap.Targets) {
		return errors.New("join allocation chain or target count mismatch")
	}
	if p.Bootstrap.Compute.Network.Metadata.Name == p.Chain.CoreNetwork {
		return errors.New("use a distinct allocation name; never collide with an existing network journal")
	}
	ids, addresses := map[string]bool{}, map[string]bool{}
	for i, t := range p.Targets {
		want := p.Bootstrap.Compute.Targets[i]
		if t.Role != "fullnode" || t.Role != want.Role || t.Name != want.Name || t.Architecture != want.Architecture || !reflect.DeepEqual(t.Images, p.Bootstrap.Targets[i].Images) || len(t.Images) != 1 || t.Images[0].Component != "core" {
			return errors.New("join supports complete Core-only fullnode allocations")
		}
		if e := t.Validate(); e != nil {
			return e
		}
		if ids[t.InstanceID] || addresses[t.PeerAddress] {
			return errors.New("duplicate target")
		}
		ids[t.InstanceID] = true
		addresses[t.PeerAddress] = true
	}
	return nil
}
func (p Plan) Request(t node.Target, action string) node.Request {
	return node.Request{Action: action, Target: t, Join: &p.Chain, Context: node.Context{PlanID: p.ID, ComputePlanID: p.Bootstrap.Compute.ID, BootstrapID: p.Bootstrap.ID, Network: p.Bootstrap.Compute.Network.Metadata.Name, CoreNetwork: p.Chain.CoreNetwork, Ports: node.DefaultPorts}}
}
