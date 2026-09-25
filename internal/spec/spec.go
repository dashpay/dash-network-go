// Package spec defines operator intent, independently of any deployment engine.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"gopkg.in/yaml.v3"
)

const Version = "dash.network/v1alpha1"

type Network struct {
	APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
	Kind       string            `json:"kind" yaml:"kind"`
	Metadata   Metadata          `json:"metadata" yaml:"metadata"`
	Chain      Chain             `json:"chain" yaml:"chain"`
	AWS        AWS               `json:"aws" yaml:"aws"`
	Nodes      []NodeGroup       `json:"nodes" yaml:"nodes"`
	Images     map[string]string `json:"images" yaml:"images"`
}

type Metadata struct {
	Name        string `json:"name" yaml:"name"`
	DisplayName string `json:"displayName" yaml:"displayName"`
	Description string `json:"description" yaml:"description"`
	Visibility  string `json:"visibility" yaml:"visibility"`
}

type Chain struct {
	Type       string `json:"type" yaml:"type"`
	Generation int    `json:"generation" yaml:"generation"`
}

type AWS struct {
	AccountID     string     `json:"accountId" yaml:"accountId"`
	Region        string     `json:"region" yaml:"region"`
	NetworkTagKey string     `json:"networkTagKey" yaml:"networkTagKey"`
	Provision     *Provision `json:"provision,omitempty" yaml:"provision,omitempty"`
}

// Provision is an explicit EC2 footprint inside existing networking. It is not
// a general infrastructure language and contains no credentials or user-data.
type Provision struct {
	StateTable       string         `json:"stateTable" yaml:"stateTable"`
	VPCID            string         `json:"vpcId" yaml:"vpcId"`
	SubnetID         string         `json:"subnetId" yaml:"subnetId"`
	SecurityGroupIDs []string       `json:"securityGroupIds" yaml:"securityGroupIds"`
	KeyName          string         `json:"keyName" yaml:"keyName"`
	PublicIPv4       bool           `json:"publicIpv4" yaml:"publicIpv4"`
	IPAMPoolID       string         `json:"ipamPoolId,omitempty" yaml:"ipamPoolId,omitempty"`
	RootVolumeGiB    int32          `json:"rootVolumeGiB" yaml:"rootVolumeGiB"`
	AMIs             map[string]AMI `json:"amis" yaml:"amis"`
}

type AMI struct {
	ID      string `json:"id" yaml:"id"`
	OwnerID string `json:"ownerId" yaml:"ownerId"`
}

type NodeGroup struct {
	Name         string `json:"name" yaml:"name"`
	Role         string `json:"role" yaml:"role"`
	Count        int    `json:"count" yaml:"count"`
	Architecture string `json:"architecture" yaml:"architecture"`
	InstanceType string `json:"instanceType" yaml:"instanceType"`
}

var slug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var account = regexp.MustCompile(`^[0-9]{12}$`)
var region = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
var instanceType = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9]+$`)

var Components = []string{"core", "dapi", "drive", "gateway", "helper", "tenderdash"}

// Load rejects unknown fields and additional YAML documents: misspelled intent
// must not silently fall back to a different deployment.
func Load(path string) (Network, error) {
	f, err := os.Open(path)
	if err != nil {
		return Network{}, err
	}
	defer f.Close()
	return Decode(f)
}

func Decode(r io.Reader) (Network, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil {
		return Network{}, err
	}
	if len(data) > 1<<20 {
		return Network{}, errors.New("network definition exceeds 1 MiB")
	}
	var n Network
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&n); err != nil {
		return n, fmt.Errorf("decode network: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return n, errors.New("network must contain exactly one YAML document")
	}
	return n, n.Validate()
}

func (n Network) Validate() error {
	if n.APIVersion != Version || n.Kind != "Network" {
		return fmt.Errorf("expected apiVersion %s and kind Network", Version)
	}
	if !slug.MatchString(n.Metadata.Name) {
		return errors.New("metadata.name must be a lowercase slug of at most 63 characters")
	}
	if n.Metadata.Visibility != "public" && n.Metadata.Visibility != "private" {
		return errors.New("metadata.visibility must explicitly be public or private")
	}
	if n.Metadata.DisplayName == "" || len(n.Metadata.DisplayName) > 100 || len(n.Metadata.Description) > 1000 {
		return errors.New("displayName is required (max 100 characters); description max 1000")
	}
	if n.Chain.Type != "devnet" && n.Chain.Type != "testnet" {
		return errors.New("chain.type must be devnet or testnet; mainnet is outside this tool's initial scope")
	}
	if n.Chain.Type == "devnet" && !strings.HasPrefix(n.Metadata.Name, "devnet-") {
		return errors.New("devnet names must start with devnet-")
	}
	if n.Chain.Type == "testnet" && strings.HasPrefix(n.Metadata.Name, "devnet-") {
		return errors.New("testnet name cannot have a devnet- prefix")
	}
	if n.Chain.Generation < 1 {
		return errors.New("chain.generation must be positive; increment it after a devnet reset")
	}
	if !account.MatchString(n.AWS.AccountID) || !region.MatchString(n.AWS.Region) {
		return errors.New("aws.accountId must have 12 digits and aws.region must be explicit")
	}
	if n.AWS.NetworkTagKey == "" || len(n.AWS.NetworkTagKey) > 128 || strings.ContainsAny(n.AWS.NetworkTagKey, "\r\n\t*") {
		return errors.New("aws.networkTagKey must be a nonempty exact tag key")
	}
	if len(n.Nodes) == 0 {
		return errors.New("at least one node group is required")
	}
	seen := map[string]bool{}
	total := 0
	for _, g := range n.Nodes {
		if !slug.MatchString(g.Name) || seen[g.Name] {
			return errors.New("node group names must be unique lowercase slugs")
		}
		seen[g.Name] = true
		switch g.Role {
		case "validator", "seed", "wallet", "miner", "fullnode":
		default:
			return fmt.Errorf("unsupported node role %q", g.Role)
		}
		if g.Count < 1 || g.Count > 1000 {
			return fmt.Errorf("node group %s count must be 1..1000", g.Name)
		}
		total += g.Count
		if g.Architecture != "amd64" && g.Architecture != "arm64" {
			return fmt.Errorf("node group %s architecture must be amd64 or arm64", g.Name)
		}
		if !instanceType.MatchString(g.InstanceType) {
			return fmt.Errorf("node group %s requires an explicit EC2 instance type", g.Name)
		}
	}
	if total > 1000 {
		return errors.New("network exceeds the initial 1000-node limit")
	}
	if n.AWS.Provision != nil {
		if err := n.ValidateProvision(); err != nil {
			return err
		}
	}
	if len(n.Images) != len(Components) {
		return fmt.Errorf("images must define exactly these components: %s", strings.Join(Components, ", "))
	}
	for _, component := range Components {
		ref := n.Images[component]
		last := ref[strings.LastIndex(ref, "/")+1:]
		if !strings.Contains(last, ":") || strings.ContainsAny(ref, " \n\r\t") {
			return fmt.Errorf("image %s requires an explicit tag or sha256 digest", component)
		}
		if _, err := name.ParseReference(ref, name.StrictValidation); err != nil {
			return fmt.Errorf("invalid %s image reference: %w", component, err)
		}
	}
	return nil
}

func (n Network) Architectures() []string {
	seen := map[string]bool{}
	for _, g := range n.Nodes {
		seen[g.Architecture] = true
	}
	result := make([]string, 0, len(seen))
	for arch := range seen {
		result = append(result, arch)
	}
	sort.Strings(result)
	return result
}

func (n Network) Fingerprint() string {
	b, _ := json.Marshal(n)
	digest := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(digest[:])
}
