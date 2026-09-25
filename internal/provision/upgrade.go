package provision

import (
	"errors"
	"fmt"
	"time"

	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
)

// Runtime images are separate from immutable creation intent. Keeping them in
// the strict shared journal also fences older executors that do not understand
// upgrades: they refuse the unknown field instead of restoring old images.
type ImageSet map[string]string
type FleetImages map[string]ImageSet

type RuntimeState struct {
	DeploymentID string      `json:"deploymentId"`
	UpgradeID    string      `json:"upgradeId"`
	Images       FleetImages `json:"images"`
}

// Preservation contains public fingerprints only, never rendered Compose or
// credentials. Core's start timestamp detects a restart with the same ID.
type Preservation struct {
	CoreID      string            `json:"coreId"`
	CoreStarted string            `json:"coreStarted"`
	CoreConfig  string            `json:"coreConfig"`
	CoreGenesis string            `json:"coreGenesis"`
	Containers  map[string]string `json:"containers,omitempty"`
	Restarts    map[string]int    `json:"restarts,omitempty"`
}

type UpgradeProgress struct {
	PlanID      string                  `json:"planId"`
	PreviousID  string                  `json:"previousId,omitempty"`
	Phase       string                  `json:"phase"`
	CurrentNode string                  `json:"currentNode,omitempty"`
	From        FleetImages             `json:"from"`
	To          FleetImages             `json:"to"`
	Baseline    map[string]Preservation `json:"baseline"`
	Completed   map[string]bool         `json:"completed"`
	ObservedAt  time.Time               `json:"observedAt,omitempty"`
}

func (images FleetImages) Validate(p Plan) error {
	if len(images) != len(p.Targets) {
		return errors.New("runtime image set lost an intended target")
	}
	for _, t := range p.Targets {
		components := []string{"core"}
		if t.Role == "validator" {
			components = spec.Components
		}
		v := images[t.Name]
		if len(v) != len(components) {
			return fmt.Errorf("incomplete runtime images for %s", t.Name)
		}
		for _, component := range components {
			ref, err := name.NewDigest(v[component], name.StrictValidation)
			if err != nil || len(ref.DigestStr()) != 71 || ref.DigestStr()[:7] != "sha256:" || !hex64.MatchString(ref.DigestStr()[7:]) {
				return fmt.Errorf("runtime image %s/%s is not an immutable SHA256 reference", t.Name, component)
			}
		}
	}
	return nil
}

func (r Record) validateUpgrade(p Plan) error {
	if r.Runtime == nil && r.Upgrade == nil {
		return nil
	}
	if r.Deployment == nil || r.Runtime == nil || r.Upgrade == nil || r.Runtime.DeploymentID != r.Deployment.PlanID || !hex64.MatchString(r.Runtime.UpgradeID) || !hex64.MatchString(r.Upgrade.PlanID) || r.Runtime.UpgradeID != r.Upgrade.PlanID {
		return errors.New("runtime/upgrade journal identity mismatch")
	}
	u := r.Upgrade
	if u.PreviousID != "" && !hex64.MatchString(u.PreviousID) {
		return errors.New("invalid previous upgrade identity")
	}
	for _, images := range []FleetImages{r.Runtime.Images, u.From, u.To} {
		if err := images.Validate(p); err != nil {
			return err
		}
	}
	switch u.Phase {
	case "staging", "applying", "verifying", "interrupted", "complete":
	default:
		return errors.New("invalid upgrade phase")
	}
	if len(u.Baseline) != len(p.Targets) {
		return errors.New("upgrade preservation baseline is incomplete")
	}
	validators := 0
	currentKnown := u.CurrentNode == ""
	for _, t := range p.Targets {
		b := u.Baseline[t.Name]
		if !hex64.MatchString(b.CoreID) || !hex64.MatchString(b.CoreConfig) || !hex64.MatchString(b.CoreGenesis) {
			return errors.New("invalid Core preservation baseline")
		}
		if _, err := time.Parse(time.RFC3339Nano, b.CoreStarted); err != nil {
			return errors.New("missing Core start evidence")
		}
		if u.From[t.Name]["core"] != u.To[t.Name]["core"] || r.Runtime.Images[t.Name]["core"] != u.From[t.Name]["core"] {
			return errors.New("upgrade attempted to alter preserved Core")
		}
		if t.Role == "validator" {
			validators++
			if _, ok := u.Completed[t.Name]; !ok {
				return errors.New("upgrade lost validator progress")
			}
			if t.Name == u.CurrentNode {
				currentKnown = true
			}
			for _, service := range []string{"drive", "tenderdash", "dapi", "gateway"} {
				if !hex64.MatchString(b.Containers[service]) || b.Restarts[service] < 0 {
					return errors.New("invalid Platform preservation baseline")
				}
			}
			if u.Phase == "complete" && !u.Completed[t.Name] {
				return errors.New("upgrade complete without every validator")
			}
		}
		for component, pin := range r.Runtime.Images[t.Name] {
			if pin != u.From[t.Name][component] && pin != u.To[t.Name][component] {
				return errors.New("runtime outside reviewed upgrade bounds")
			}
			if u.Phase == "complete" && pin != u.To[t.Name][component] {
				return errors.New("completed upgrade differs from desired images")
			}
		}
	}
	if !currentKnown || len(u.Completed) != validators {
		return errors.New("invalid upgrade target scope")
	}
	if u.Phase == "complete" && (u.CurrentNode != "" || u.ObservedAt.IsZero()) {
		return errors.New("upgrade lacks final verification")
	}
	return nil
}
