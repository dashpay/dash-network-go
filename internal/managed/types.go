// Package managed operates explicitly enrolled, existing Dash workloads. Import
// never rewrites identities, tags, configuration or containers.
package managed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
)

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var slugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var idRE = regexp.MustCompile(`^i-([0-9a-f]{8}|[0-9a-f]{17})$`)
var containerRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var accountRE = regexp.MustCompile(`^[0-9]{12}$`)
var regionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
var tableRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,255}$`)
var Components = []string{"core", "drive", "tenderdash", "dapi", "gateway", "helper"}

type Fleet struct {
	APIVersion    string           `json:"apiVersion"`
	Kind          string           `json:"kind"`
	Metadata      spec.Metadata    `json:"metadata"`
	ChainType     string           `json:"chainType"`
	CoreNetwork   string           `json:"coreNetwork"`
	AccountID     string           `json:"accountId"`
	Region        string           `json:"region"`
	NetworkTagKey string           `json:"networkTagKey"`
	StateTable    string           `json:"stateTable"`
	Access        bootstrap.Access `json:"access"`
	Targets       []Target         `json:"targets"`
}
type Target struct {
	Name         string            `json:"name"`
	InstanceID   string            `json:"instanceId"`
	Address      string            `json:"address"`
	Architecture string            `json:"architecture"`
	Role         string            `json:"role"`
	Containers   map[string]string `json:"containers"`
}

func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func (f Fleet) ID() string  { return hash(f) }
func (f Fleet) Key() string { return f.AccountID + "/" + f.Region + "/" + f.Metadata.Name }
func (f Fleet) Validate() error {
	if f.APIVersion != spec.Version || f.Kind != "ExistingNetwork" || !slugRE.MatchString(f.Metadata.Name) || f.Metadata.DisplayName == "" || len(f.Metadata.Description) > 1000 || (f.Metadata.Visibility != "private" && f.Metadata.Visibility != "public") {
		return errors.New("explicit existing-network identity/visibility required")
	}
	if f.ChainType != "testnet" && f.ChainType != "devnet" {
		return errors.New("only managed testnet or devnet supported")
	}
	if (f.ChainType == "testnet" && f.CoreNetwork != "test") || (f.ChainType == "devnet" && (!strings.HasPrefix(f.CoreNetwork, "devnet-") || !slugRE.MatchString(f.CoreNetwork))) {
		return errors.New("explicit Core chain name required")
	}
	if !accountRE.MatchString(f.AccountID) || !regionRE.MatchString(f.Region) || !tableRE.MatchString(f.StateTable) || f.NetworkTagKey == "" || strings.ContainsAny(f.NetworkTagKey, "*\r\n\t") {
		return errors.New("explicit AWS account/region/tag/journal required")
	}
	if f.Access.User != "ubuntu" && f.Access.User != "root" {
		return errors.New("supported SSH user is ubuntu or root")
	}
	if f.Access.Port < 1 || f.Access.Port > 65535 || len(f.Targets) == 0 || len(f.Targets) > 200 {
		return errors.New("invalid SSH port or target count")
	}
	names, ids, addresses := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, t := range f.Targets {
		ip := net.ParseIP(t.Address)
		if !slugRE.MatchString(t.Name) || !idRE.MatchString(t.InstanceID) || names[t.Name] || ids[t.InstanceID] || addresses[t.Address] || ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("targets require unique names/instances and usable IPv4 addresses")
		}
		names[t.Name] = true
		ids[t.InstanceID] = true
		addresses[t.Address] = true
		if t.Architecture != "amd64" && t.Architecture != "arm64" {
			return errors.New("invalid architecture")
		}
		if t.Role != "validator" && t.Role != "seed" && t.Role != "core" {
			return errors.New("supported roles: validator, seed, core")
		}
		containers := map[string]bool{}
		for c, n := range t.Containers {
			if !slices.Contains(Components, c) || !containerRE.MatchString(n) || containers[n] {
				return errors.New("invalid/duplicate component container")
			}
			containers[n] = true
		}
		if t.Role == "validator" {
			for _, c := range []string{"core", "drive", "tenderdash", "dapi", "gateway"} {
				if t.Containers[c] == "" {
					return fmt.Errorf("validator %s missing %s", t.Name, c)
				}
			}
		}
		if (t.Role == "core" && (len(t.Containers) != 1 || t.Containers["core"] == "")) || (t.Role == "seed" && t.Containers["tenderdash"] == "") {
			return errors.New("role lacks required workload")
		}
	}
	return nil
}

type Container struct {
	ID         string   `json:"id"`
	ImageID    string   `json:"imageId"`
	Image      string   `json:"image"`
	Digests    []string `json:"digests"`
	ConfigHash string   `json:"configHash"`
	Running    bool     `json:"running"`
	StartedAt  string   `json:"startedAt"`
	Restarts   int      `json:"restarts"`
}
type Chain struct {
	CoreNetwork      string           `json:"coreNetwork"`
	CoreGenesis      string           `json:"coreGenesis"`
	CoreHeight       int64            `json:"coreHeight"`
	CoreSynced       bool             `json:"coreSynced"`
	ChainLockHeight  int64            `json:"chainLockHeight"`
	MasternodeState  string           `json:"masternodeState"`
	ProTxHash        string           `json:"proTxHash"`
	PlatformChainID  string           `json:"platformChainId"`
	PlatformHeight   int64            `json:"platformHeight"`
	PlatformHash     string           `json:"platformHash"`
	PlatformProtocol uint32           `json:"platformProtocol"`
	PlatformNodeID   string           `json:"platformNodeId"`
	CatchingUp       bool             `json:"catchingUp"`
	VotingPower      map[string]int64 `json:"votingPower"`
	DAPIHeight       int64            `json:"dapiHeight"`
	DAPIHealthy      bool             `json:"dapiHealthy"`
}
type Observation struct {
	InstanceID      string               `json:"instanceId"`
	At              time.Time            `json:"at"`
	Components      map[string]Container `json:"components"`
	Companions      map[string]Container `json:"companions"`
	NativeProcesses map[string]string    `json:"nativeProcesses"`
	FilesHash       string               `json:"filesHash"`
	Chain           Chain                `json:"chain"`
	Problems        []string             `json:"problems"`
	Error           string               `json:"error,omitempty"`
}
type Snapshot struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	ID         string                 `json:"id"`
	Fleet      Fleet                  `json:"fleet"`
	ObservedAt time.Time              `json:"observedAt"`
	Nodes      map[string]Observation `json:"nodes"`
}

func (s Snapshot) Validate() error {
	copy := s
	copy.ID = ""
	if s.APIVersion != spec.Version || s.Kind != "ExistingSnapshot" || s.ID != hash(copy) || s.ObservedAt.IsZero() {
		return errors.New("invalid/altered existing snapshot")
	}
	if err := s.Fleet.Validate(); err != nil {
		return err
	}
	if len(s.Nodes) != len(s.Fleet.Targets) {
		return errors.New("snapshot lost target")
	}
	for _, t := range s.Fleet.Targets {
		o, ok := s.Nodes[t.Name]
		if !ok || o.InstanceID != t.InstanceID {
			return errors.New("snapshot target identity mismatch")
		}
	}
	return nil
}
func (s Snapshot) Complete() error {
	if err := s.Validate(); err != nil {
		return err
	}
	var genesis, chain string
	for _, t := range s.Fleet.Targets {
		o := s.Nodes[t.Name]
		if o.Error != "" || len(o.Problems) > 0 || !digestRE.MatchString(o.FilesHash) || len(o.Components) != len(t.Containers) {
			return fmt.Errorf("%s: incomplete/unsupported observation", t.Name)
		}
		for c := range t.Containers {
			v, ok := o.Components[c]
			if !ok || !digestRE.MatchString(v.ID) || !digestRE.MatchString(v.ConfigHash) || !strings.HasPrefix(v.ImageID, "sha256:") {
				return errors.New("invalid workload fingerprint")
			}
		}
		if t.Containers["core"] != "" {
			if o.Chain.CoreNetwork != s.Fleet.CoreNetwork || !digestRE.MatchString(o.Chain.CoreGenesis) {
				return errors.New("Core identity not proved")
			}
			if genesis != "" && genesis != o.Chain.CoreGenesis {
				return errors.New("Core genesis differs across fleet")
			}
			genesis = o.Chain.CoreGenesis
		}
		if t.Role == "validator" {
			if o.Chain.PlatformChainID == "" || !digestRE.MatchString(o.Chain.ProTxHash) || o.Chain.PlatformNodeID == "" {
				return errors.New("validator identity not proved")
			}
			if chain != "" && chain != o.Chain.PlatformChainID {
				return errors.New("Platform chain differs across fleet")
			}
			chain = o.Chain.PlatformChainID
		}
	}
	return nil
}

// Images are already architecture-specific; discovery/resolution must finish
// before a reviewed plan. No mutable tag is accepted by an executor.
type Images map[string]map[string]string

func validPin(pin string) bool {
	d, e := name.NewDigest(pin, name.StrictValidation)
	return e == nil && strings.HasPrefix(d.DigestStr(), "sha256:") && digestRE.MatchString(strings.TrimPrefix(d.DigestStr(), "sha256:"))
}

type Plan struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	Snapshot   Snapshot  `json:"snapshot"`
	Operation  string    `json:"operation"`
	Scope      string    `json:"scope"`
	Images     Images    `json:"images"`
	PreviousID string    `json:"previousId,omitempty"`
	Recipe     string    `json:"recipe"`
	CreatedAt  time.Time `json:"createdAt"`
}

func Selected(scope, component string) bool {
	return scope == "all" || (scope == "core" && component == "core") || (scope == "platform" && component != "core" && component != "helper") || (scope == "tenderdash" && component == "tenderdash")
}
func Build(s Snapshot, operation, scope, previous string, images Images, now time.Time) (Plan, error) {
	p := Plan{APIVersion: spec.Version, Kind: "ExistingOperation", Snapshot: s, Operation: operation, Scope: scope, PreviousID: previous, Images: images, Recipe: RecipeDigest(), CreatedAt: now.UTC()}
	p.ID = hash(p)
	return p, p.Validate()
}
func (p Plan) Validate() error {
	c := p
	c.ID = ""
	if p.APIVersion != spec.Version || p.Kind != "ExistingOperation" || p.ID != hash(c) || p.Recipe != RecipeDigest() || p.CreatedAt.IsZero() {
		return errors.New("managed plan altered or executor recipe changed")
	}
	if err := p.Snapshot.Complete(); err != nil {
		return err
	}
	if p.PreviousID != "" && !digestRE.MatchString(p.PreviousID) {
		return errors.New("invalid predecessor")
	}
	if p.Operation != "upgrade" && p.Operation != "deploy" {
		return errors.New("supported existing operations: deploy, upgrade")
	}
	if !slices.Contains([]string{"core", "platform", "tenderdash", "all"}, p.Scope) {
		return errors.New("unsupported component scope")
	}
	count := 0
	for _, t := range p.Snapshot.Fleet.Targets {
		pins := p.Images[t.Name]
		expected := 0
		for c := range t.Containers {
			if Selected(p.Scope, c) {
				expected++
				if !validPin(pins[c]) {
					return fmt.Errorf("missing immutable image %s/%s", t.Name, c)
				}
			}
		}
		if len(pins) != expected {
			return errors.New("image selection exceeds scope")
		}
		if expected > 0 {
			count++
		}
	}
	if len(p.Images) != len(p.Snapshot.Fleet.Targets) || count == 0 {
		return errors.New("image selection lost target or selects nothing")
	}
	return nil
}
