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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
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
	// PremineHeight is mined at minimum difficulty before any EvoNode is
	// registered, as long-running devnets do (minimumdifficultyblocks=4032):
	// quorums and Platform then start on a mature chain. Omitted in older plans.
	PremineHeight int `json:"premineHeight,omitempty"`
	// PlatformEpochSeconds is the Platform epoch length (Drive's
	// EPOCH_TIME_LENGTH_S). Omitted in older plans, which run 3600.
	PlatformEpochSeconds int `json:"platformEpochSeconds,omitempty"`
	// Advertise "public": masternodes register, and Core/Tenderdash advertise,
	// the IPAM Elastic IP of each host, as long-running devnets do; clients
	// outside the VPC can then use the masternode list. Empty: private VPC
	// addresses (older plans, or networks without IPAM addresses).
	Advertise string `json:"advertise,omitempty"`
	// GatewayTLS: validators obtain and renew publicly trusted certificates
	// for their public IPs (ACME HTTP-01 on port 80), as long-running devnets'
	// gateways do. Omitted: persisted self-signed certificates.
	GatewayTLS *node.GatewayTLS `json:"gatewayTls,omitempty"`
	Targets    []node.Target    `json:"targets"`
}

// Options select the address mode and gateway certificates of a new plan.
type Options struct {
	// Public maps every target to its journaled IPAM Elastic IP; nil keeps
	// private VPC addresses.
	Public map[string]string
	// ACMEIssuer and ACMEEmail request trusted gateway certificates from the
	// release lock's acme image; requires public addresses.
	ACMEIssuer, ACMEEmail string
	// BlockSeconds is the Core block interval: Core's powtargetspacing and the
	// miner's cadence. Zero keeps the default.
	BlockSeconds int
	// EpochSeconds is the Platform epoch length. Zero keeps the default.
	EpochSeconds int
}

// DefaultEpochSeconds and the supported range of Platform epoch lengths.
const (
	DefaultEpochSeconds = 3600
	MinEpochSeconds     = 60
	MaxEpochSeconds     = 30 * 24 * 3600
)

// DefaultBlockSeconds and the supported range of Core block intervals. DKG,
// ChainLock and upgrade timing follow powtargetspacing. With more than three
// peers Core finishes a node's blockchain sync only after more than six whole
// seconds without a new block; faster blocks would leave any restarted node
// unsynced, ignoring MNAUTH (one-sided quorum links, PoSe).
const (
	DefaultBlockSeconds = 10
	MinBlockSeconds     = 8
	MaxBlockSeconds     = 600
)

// DefaultPremineHeight matches the legacy devnet tooling.
const DefaultPremineHeight = 4032

func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Build binds private VPC service addresses.
func Build(b bootstrap.Plan, live map[string]types.Instance, protocol uint32, now time.Time) (Plan, error) {
	return BuildAdvertising(b, live, nil, protocol, now)
}

// BuildAdvertising binds public service addresses when public maps every
// target to its journaled IPAM Elastic IP; nil keeps private addresses.
func BuildAdvertising(b bootstrap.Plan, live map[string]types.Instance, public map[string]string, protocol uint32, now time.Time) (Plan, error) {
	return BuildWith(b, live, Options{Public: public}, protocol, now)
}

func BuildWith(b bootstrap.Plan, live map[string]types.Instance, o Options, protocol uint32, now time.Time) (Plan, error) {
	public := o.Public
	if err := b.Validate(); err != nil {
		return Plan{}, err
	}
	p := Plan{APIVersion: spec.Version, Kind: "DevnetDeploymentPlan", Bootstrap: b, Profile: Profile, RecipeSHA256: node.RecipeDigest(), InitialProtocolVersion: protocol, MiningIntervalSeconds: DefaultBlockSeconds, PremineHeight: DefaultPremineHeight, GenesisTime: now.UTC()}
	if o.BlockSeconds != 0 {
		p.MiningIntervalSeconds = o.BlockSeconds
	}
	p.PlatformEpochSeconds = DefaultEpochSeconds
	if o.EpochSeconds != 0 {
		p.PlatformEpochSeconds = o.EpochSeconds
	}
	p.CoreNetwork = fmt.Sprintf("%s-g%d", strings.TrimPrefix(b.Compute.Network.Metadata.Name, "devnet-"), b.Compute.Network.Chain.Generation)
	p.PlatformChainID = "dash-devnet-" + p.CoreNetwork
	if public != nil {
		p.Advertise = "public"
	}
	if o.ACMEIssuer != "" {
		tls, err := gatewayTLS(b, o.ACMEIssuer, o.ACMEEmail)
		if err != nil {
			return Plan{}, err
		}
		p.GatewayTLS = tls
	}
	for i, t := range b.Compute.Targets {
		instance, ok := live[t.Name]
		if !ok {
			return Plan{}, fmt.Errorf("missing target %s", t.Name)
		}
		ip := aws.ToString(instance.PrivateIpAddress)
		if b.Access.Address == "public" {
			ip = aws.ToString(instance.PublicIpAddress)
		}
		target := node.Target{Name: t.Name, Role: t.Role, Architecture: t.Architecture, InstanceID: aws.ToString(instance.InstanceId), SSHAddress: ip, PeerAddress: aws.ToString(instance.PrivateIpAddress), Images: b.Targets[i].Images}
		if public != nil {
			// The Elastic IP must be the one this network allocated from IPAM and
			// currently associated with exactly this instance.
			eip := aws.ToString(instance.PublicIpAddress)
			if public[t.Name] == "" || public[t.Name] != eip {
				return Plan{}, fmt.Errorf("%s: public address %q is not its journaled IPAM Elastic IP", t.Name, eip)
			}
			target.PeerAddress, target.PrivateAddress = eip, aws.ToString(instance.PrivateIpAddress)
		}
		p.Targets = append(p.Targets, target)
	}
	p.ID = hash(p)
	return p, p.Validate()
}
func (p Plan) Validate() error {
	if p.Bootstrap.Compute.Network.Chain.Type != "devnet" {
		return errors.New("genesis lifecycle is devnet-only; existing chains require a join plan")
	}
	if err := p.Bootstrap.Validate(); err != nil {
		return err
	}
	copy := p
	copy.ID = ""
	if p.ID != hash(copy) || p.APIVersion != spec.Version || p.Kind != "DevnetDeploymentPlan" || p.Profile != Profile || p.RecipeSHA256 != node.RecipeDigest() {
		return errors.New("deployment plan altered or node recipe changed; retain exact plan/binary")
	}
	if p.InitialProtocolVersion < 1 || p.InitialProtocolVersion > 100 || p.GenesisTime.IsZero() || p.MiningIntervalSeconds < MinBlockSeconds || p.MiningIntervalSeconds > MaxBlockSeconds || p.PremineHeight < 0 || p.PremineHeight > 20000 {
		return errors.New("explicit protocol version (1..100), genesis time and supported mining policy required")
	}
	if p.PlatformEpochSeconds != 0 && (p.PlatformEpochSeconds < MinEpochSeconds || p.PlatformEpochSeconds > MaxEpochSeconds) {
		return fmt.Errorf("Platform epoch must be %d..%d seconds", MinEpochSeconds, MaxEpochSeconds)
	}
	coreNetwork := fmt.Sprintf("%s-g%d", strings.TrimPrefix(p.Bootstrap.Compute.Network.Metadata.Name, "devnet-"), p.Bootstrap.Compute.Network.Chain.Generation)
	if p.CoreNetwork != coreNetwork || p.PlatformChainID != "dash-devnet-"+coreNetwork || len(p.PlatformChainID) > 50 {
		return errors.New("invalid or overlong chain identity; use a shorter devnet name")
	}
	if len(p.Targets) != len(p.Bootstrap.Targets) {
		return errors.New("deployment target set incomplete")
	}
	if p.Advertise != "" && p.Advertise != "public" {
		return errors.New("advertise must be public or omitted (private)")
	}
	if p.GatewayTLS != nil {
		if p.Advertise != "public" {
			return errors.New("trusted gateway certificates require public service addresses")
		}
		expected, err := gatewayTLS(p.Bootstrap, p.GatewayTLS.Issuer, p.GatewayTLS.Email)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(expected, p.GatewayTLS) {
			return errors.New("gateway TLS ACME client images differ from the release lock")
		}
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
		if err := advertised(p.Advertise, t); err != nil {
			return err
		}
		if ids[t.InstanceID] || ips[t.PeerAddress] || (t.PrivateAddress != "" && ips[t.PrivateAddress]) {
			return errors.New("duplicate instance or peer address")
		}
		ids[t.InstanceID] = true
		ips[t.PeerAddress] = true
		if t.PrivateAddress != "" {
			ips[t.PrivateAddress] = true
		}
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

// gatewayTLS pins the release lock's acme image for every validator architecture.
func gatewayTLS(b bootstrap.Plan, issuer, email string) (*node.GatewayTLS, error) {
	tls := &node.GatewayTLS{Issuer: issuer, Email: email, Images: map[string]string{}}
	var archs []string
	for _, t := range b.Compute.Targets {
		if t.Role == "validator" && !slices.Contains(archs, t.Architecture) {
			archs = append(archs, t.Architecture)
		}
	}
	for _, image := range b.Release.Images {
		if image.Component != "acme" {
			continue
		}
		repo, err := name.NewDigest(image.Pinned, name.StrictValidation)
		if err != nil {
			return nil, err
		}
		for _, platform := range image.Platforms {
			if slices.Contains(archs, platform.Architecture) {
				tls.Images[platform.Architecture] = repo.Context().Digest(platform.Digest).Name()
			}
		}
	}
	if len(tls.Images) == 0 {
		return nil, errors.New("trusted gateway certificates need an acme image (for example goacme/lego) in the network images")
	}
	return tls, tls.Validate(archs)
}

// advertised checks that a target's addresses match the plan's address mode.
func advertised(mode string, t node.Target) error {
	peer, private := net.ParseIP(t.PeerAddress), net.ParseIP(t.PrivateAddress)
	if mode == "public" {
		if peer == nil || peer.IsPrivate() || !peer.IsGlobalUnicast() || private == nil || !private.IsPrivate() {
			return fmt.Errorf("%s: public advertising requires a public peer address and a private VPC address", t.Name)
		}
		return nil
	}
	if t.PrivateAddress != "" {
		return fmt.Errorf("%s: a private-address plan carries no separate private address", t.Name)
	}
	return nil
}

// VPCAddress is the address bound on the instance's interface.
func VPCAddress(t node.Target) string {
	if t.PrivateAddress != "" {
		return t.PrivateAddress
	}
	return t.PeerAddress
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
