package lifecycle

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
)

func (r Runner) PrepareUpgrade(ctx context.Context, p Plan, record provision.Record, candidate spec.Network, lock release.Lock, scope string) (UpgradePlan, error) {
	u, err := BuildUpgrade(p, record, candidate, lock, scope, time.Now())
	if err != nil {
		return u, err
	}
	if err = liveScope(ctx, p, record, r.Cloud); err != nil {
		return UpgradePlan{}, err
	}
	return u, nil
}

// UpgradePlan changes images on the exact owned deployment. It cannot adopt
// existing testnet nodes, alter protocol/genesis, reset data or replace Core.
type UpgradePlan struct {
	APIVersion   string                `json:"apiVersion"`
	Kind         string                `json:"kind"`
	ID           string                `json:"id"`
	Deployment   Plan                  `json:"deployment"`
	Candidate    spec.Network          `json:"candidate"`
	Release      release.Lock          `json:"release"`
	Scope        string                `json:"scope"`
	PreviousID   string                `json:"previousId,omitempty"`
	RecipeSHA256 string                `json:"recipeSha256"`
	CreatedAt    time.Time             `json:"createdAt"`
	From         provision.FleetImages `json:"from"`
	To           provision.FleetImages `json:"to"`
	Recovery     string                `json:"recovery"`
}

const upgradeRecovery = "forward-only; stop on failure; no automatic downgrade, database reset or protocol migration"

func upgradeTargets(p Plan, candidate spec.Network, lock release.Lock, from provision.FleetImages, scope string) (provision.FleetImages, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	if err := lock.Validate(candidate); err != nil {
		return nil, err
	}
	if err := from.Validate(p.Bootstrap.Compute); err != nil {
		return nil, err
	}
	unchanged := candidate
	unchanged.Images = p.Bootstrap.Compute.Network.Images
	if !reflect.DeepEqual(unchanged, p.Bootstrap.Compute.Network) {
		return nil, errors.New("upgrade cannot change network identity, topology, cloud placement or metadata")
	}
	if scope != "platform" && scope != "tenderdash" {
		return nil, errors.New("executable upgrades currently support platform or tenderdash scope only")
	}
	to := cloneImages(from)
	changed := false
	for _, t := range p.Targets {
		for _, image := range t.Images {
			if image.Component == "core" && from[t.Name]["core"] != image.Pinned {
				return nil, errors.New("upgrade source differs from preserved Core image")
			}
		}
		if t.Role != "validator" {
			continue
		}
		for _, image := range lock.Images {
			if image.Component == "core" || (scope == "tenderdash" && image.Component != "tenderdash") {
				continue
			}
			repo, err := name.NewDigest(image.Pinned, name.StrictValidation)
			if err != nil {
				return nil, err
			}
			for _, platform := range image.Platforms {
				if platform.Architecture == t.Architecture {
					pin := repo.Context().Digest(platform.Digest).Name()
					changed = changed || pin != from[t.Name][image.Component]
					to[t.Name][image.Component] = pin
				}
			}
		}
	}
	if !changed {
		return nil, errors.New("selected runtime images are unchanged; no upgrade operation needed")
	}
	return to, to.Validate(p.Bootstrap.Compute)
}

func BuildUpgrade(p Plan, record provision.Record, candidate spec.Network, lock release.Lock, scope string, now time.Time) (UpgradePlan, error) {
	if err := record.Validate(p.Bootstrap.Compute); err != nil {
		return UpgradePlan{}, err
	}
	if record.Deployment == nil || record.Deployment.PlanID != p.ID || record.Deployment.Phase != "network-ready" {
		return UpgradePlan{}, errors.New("complete the original deployment before planning an upgrade")
	}
	if record.Upgrade != nil && record.Upgrade.Phase != "complete" {
		return UpgradePlan{}, errors.New("resume the unfinished upgrade; do not supersede it")
	}
	from, err := effectiveImages(p, record)
	if err != nil {
		return UpgradePlan{}, err
	}
	to, err := upgradeTargets(p, candidate, lock, from, scope)
	if err != nil {
		return UpgradePlan{}, err
	}
	u := UpgradePlan{APIVersion: spec.Version, Kind: "DevnetUpgradePlan", Deployment: p, Candidate: candidate, Release: lock, Scope: scope, CreatedAt: now.UTC(), From: from, To: to, RecipeSHA256: node.UpgradeDigest(), Recovery: upgradeRecovery}
	if record.Runtime != nil {
		u.PreviousID = record.Runtime.UpgradeID
	}
	u.ID = hash(u)
	return u, u.Validate()
}

func (u UpgradePlan) Validate() error {
	copy := u
	copy.ID = ""
	if u.ID != hash(copy) || u.APIVersion != spec.Version || u.Kind != "DevnetUpgradePlan" || u.RecipeSHA256 != node.UpgradeDigest() || u.Recovery != upgradeRecovery || u.CreatedAt.IsZero() {
		return errors.New("upgrade plan altered or recipe changed; retain exact plan/binary")
	}
	if u.PreviousID != "" && (len(u.PreviousID) != 64 || u.PreviousID == u.ID) {
		return errors.New("invalid upgrade predecessor")
	}
	if _, err := hex.DecodeString(u.PreviousID); err != nil {
		return errors.New("invalid upgrade predecessor digest")
	}
	to, err := upgradeTargets(u.Deployment, u.Candidate, u.Release, u.From, u.Scope)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(to, u.To) {
		return errors.New("upgrade targets differ from reviewed release/scope")
	}
	return nil
}
