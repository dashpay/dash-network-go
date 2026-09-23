// Package plan describes bounded operations. It does not execute them.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/spec"
)

type Plan struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	ID         string           `json:"id"`
	Network    string           `json:"network"`
	Generation int              `json:"generation"`
	AccountID  string           `json:"accountId"`
	Region     string           `json:"region"`
	SpecHash   string           `json:"specHash"`
	Operation  string           `json:"operation"`
	Scope      string           `json:"scope"`
	Executable bool             `json:"executable"`
	Targets    []spec.NodeGroup `json:"targets"`
	Images     []release.Image  `json:"images"`
	Preserve   []string         `json:"preserve"`
	Steps      []string         `json:"steps"`
	Warnings   []string         `json:"warnings"`
}

func Build(n spec.Network, lock release.Lock, operation, scope string) (Plan, error) {
	if err := n.Validate(); err != nil {
		return Plan{}, err
	}
	if err := lock.Validate(n); err != nil {
		return Plan{}, err
	}
	if operation != "create" && operation != "upgrade" {
		return Plan{}, errors.New("operation must be create or upgrade")
	}
	if operation == "create" && (n.Chain.Type != "devnet" || scope != "all") {
		return Plan{}, errors.New("create is devnet-only and requires scope all; existing testnet is never bootstrapped/reset by create")
	}
	selected := map[string]bool{}
	switch scope {
	case "all":
		for _, c := range spec.Components {
			selected[c] = true
		}
	case "platform":
		for _, c := range spec.Components {
			if c != "core" {
				selected[c] = true
			}
		}
	case "core", "tenderdash":
		selected[scope] = true
	default:
		return Plan{}, errors.New("scope must be all, platform, core, or tenderdash")
	}
	p := Plan{APIVersion: spec.Version, Kind: "OperationPlan", Network: n.Metadata.Name,
		Generation: n.Chain.Generation, AccountID: n.AWS.AccountID, Region: n.AWS.Region,
		SpecHash: n.Fingerprint(), Operation: operation, Scope: scope, Preserve: []string{},
		Warnings: []string{
			"Planning foundation only: this intent plan cannot be applied; no infrastructure or node changes were made.",
			"Artifact availability does not prove runtime, protocol, database, or upgrade compatibility.",
			"Live target membership, current versions, quorum safety, and cloud prerequisites are not evaluated by this offline plan.",
		}}
	for _, image := range lock.Images {
		if selected[image.Component] {
			p.Images = append(p.Images, image)
		}
	}
	sort.Slice(p.Images, func(i, j int) bool { return p.Images[i].Component < p.Images[j].Component })
	for _, group := range n.Nodes {
		if scope == "all" || scope == "core" || group.Role == "validator" || group.Role == "seed" {
			p.Targets = append(p.Targets, group)
		}
	}
	if len(p.Targets) == 0 {
		return Plan{}, fmt.Errorf("scope %s has no applicable node groups", scope)
	}
	sort.Slice(p.Targets, func(i, j int) bool { return p.Targets[i].Name < p.Targets[j].Name })
	p.Steps = []string{"verify-account-and-live-targets", "acquire-network-lock", "record-current-state", "check-upgrade-compatibility", "stage-pinned-images"}
	if operation == "create" {
		p.Steps = []string{"verify-account-and-capacity", "acquire-network-lock", "create-owned-resources", "prepare-hosts", "start-core", "wait-for-chain-readiness", "fund-and-register-nodes", "start-platform", "verify-network"}
	} else {
		p.Preserve = []string{"network-identity", "node-identities", "persistent-data", "unselected-components"}
		if !selected["core"] {
			p.Preserve = append(p.Preserve, "core-processes-and-configuration")
		}
		p.Steps = append(p.Steps, "canary", "health-gated-rollout", "verify-network-and-preservation")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return Plan{}, err
	}
	h := sha256.Sum256(b)
	p.ID = "plan-" + hex.EncodeToString(h[:12])
	return p, nil
}
