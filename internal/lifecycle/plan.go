// Package lifecycle runs the owned devnet chain lifecycle using typed node
// operations. The former Ansible deployment is reference material, never a backend.
package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/spec"
)

const Profile = "devnet-core23-platform4-tenderdash1"

type Plan struct {
	APIVersion             string         `json:"apiVersion"`
	Kind                   string         `json:"kind"`
	ID                     string         `json:"id"`
	Bootstrap              bootstrap.Plan `json:"bootstrap"`
	Profile                string         `json:"profile"`
	RecipeSHA256           string         `json:"recipeSha256"`
	CoreNetwork            string         `json:"coreNetwork"`
	PlatformChainID        string         `json:"platformChainId"`
	GenesisTime            time.Time      `json:"genesisTime"`
	InitialProtocolVersion uint32         `json:"initialProtocolVersion"`
	MiningIntervalSeconds  int            `json:"miningIntervalSeconds"`
	Targets                []node.Target  `json:"targets"`
}

func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func Build(b bootstrap.Plan, live map[string]types.Instance, protocol uint32, now time.Time) (Plan, error) {
	if err := b.Validate(); err != nil {
		return Plan{}, err
	}
	p := Plan{APIVersion: spec.Version, Kind: "DevnetDeploymentPlan", Bootstrap: b, Profile: Profile, RecipeSHA256: node.RecipeDigest(), InitialProtocolVersion: protocol, MiningIntervalSeconds: 10, GenesisTime: now.UTC()}
	p.CoreNetwork = fmt.Sprintf("%s-g%d", strings.TrimPrefix(b.Compute.Network.Metadata.Name, "devnet-"), b.Compute.Network.Chain.Generation)
	p.PlatformChainID = "dash-devnet-" + p.CoreNetwork
	for i, t := range b.Compute.Targets {
		instance, ok := live[t.Name]
		if !ok {
			return Plan{}, fmt.Errorf("missing target %s", t.Name)
		}
		ip := aws.ToString(instance.PrivateIpAddress)
		if b.Access.Address == "public" {
			ip = aws.ToString(instance.PublicIpAddress)
		}
		// Use the actual private VPC network for peer traffic; Core explicitly allows
		// private devnet addresses. Public exposure still follows the existing SGs.
		p.Targets = append(p.Targets, node.Target{Name: t.Name, Role: t.Role, Architecture: t.Architecture, InstanceID: aws.ToString(instance.InstanceId), SSHAddress: ip, PeerAddress: aws.ToString(instance.PrivateIpAddress), Images: b.Targets[i].Images})
	}
	p.ID = hash(p)
	return p, p.Validate()
}
func (p Plan) Validate() error {
	if err := p.Bootstrap.Validate(); err != nil {
		return err
	}
	copy := p
	copy.ID = ""
	if p.ID != hash(copy) || p.APIVersion != spec.Version || p.Kind != "DevnetDeploymentPlan" || p.Profile != Profile || p.RecipeSHA256 != node.RecipeDigest() {
		return errors.New("deployment plan altered or node recipe changed; retain exact plan/binary")
	}
	if p.InitialProtocolVersion < 1 || p.InitialProtocolVersion > 100 || p.GenesisTime.IsZero() || p.MiningIntervalSeconds != 10 {
		return errors.New("explicit protocol version (1..100), genesis time and supported mining policy required")
	}
	coreNetwork := fmt.Sprintf("%s-g%d", strings.TrimPrefix(p.Bootstrap.Compute.Network.Metadata.Name, "devnet-"), p.Bootstrap.Compute.Network.Chain.Generation)
	if p.CoreNetwork != coreNetwork || p.PlatformChainID != "dash-devnet-"+coreNetwork || len(p.PlatformChainID) > 50 {
		return errors.New("invalid or overlong chain identity; use a shorter devnet name")
	}
	if len(p.Targets) != len(p.Bootstrap.Targets) {
		return errors.New("deployment target set incomplete")
	}
	counts := map[string]int{}
	ids := map[string]bool{}
	ips := map[string]bool{}
	for i, t := range p.Targets {
		expected := p.Bootstrap.Compute.Targets[i]
		if t.Name != expected.Name || t.Role != expected.Role || t.Architecture != expected.Architecture || !reflect.DeepEqual(t.Images, p.Bootstrap.Targets[i].Images) {
			return errors.New("deployment differs from the approved bootstrap target")
		}
		if err := t.Validate(); err != nil {
			return err
		}
		if ids[t.InstanceID] || ips[t.PeerAddress] {
			return errors.New("duplicate instance or peer address")
		}
		ids[t.InstanceID] = true
		ips[t.PeerAddress] = true
		if t.Role != "validator" && t.Role != "wallet" && t.Role != "miner" && t.Role != "fullnode" {
			return errors.New("full devnet profile supports validator, wallet, miner and fullnode roles; seeds must not be silently omitted")
		}
		counts[t.Role]++
	}
	// Default devnet quorums: Platform 12/9/8, ChainLocks 12/7/6,
	// rotated InstantSend 8/6/4. Thirteen validators gives one spare member.
	if counts["validator"] < 13 || counts["validator"] > 25 || counts["wallet"] != 1 || counts["miner"] > 1 {
		return errors.New("full devnet profile requires 13..25 validators, exactly one wallet, and at most one separate miner")
	}
	return nil
}
func (p Plan) Wallet() node.Target {
	for _, t := range p.Targets {
		if t.Role == "wallet" {
			return t
		}
	}
	panic("validated plan missing wallet")
}
func (p Plan) Miner() node.Target {
	for _, t := range p.Targets {
		if t.Role == "miner" {
			return t
		}
	}
	return p.Wallet()
}
func (p Plan) Validators() []node.Target {
	var out []node.Target
	for _, t := range p.Targets {
		if t.Role == "validator" {
			out = append(out, t)
		}
	}
	return out
}
func (p Plan) PeerAddresses() []string {
	var out []string
	for _, t := range p.Targets {
		out = append(out, net.JoinHostPort(t.PeerAddress, "20001"))
	}
	sort.Strings(out)
	return out
}
