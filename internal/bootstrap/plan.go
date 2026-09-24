// Package bootstrap prepares owned devnet nodes; it does not start Dash services.
package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"

	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
)

type Access struct {
	User    string `json:"user"`
	Port    int    `json:"port"`
	Address string `json:"address"` // private or public; actual IPs come from EC2.
}

type Plan struct {
	APIVersion   string         `json:"apiVersion"`
	Kind         string         `json:"kind"`
	ID           string         `json:"id"`
	Compute      provision.Plan `json:"compute"`
	Release      release.Lock   `json:"release"`
	Access       Access         `json:"access"`
	RecipeSHA256 string         `json:"recipeSha256"`
	Targets      []Target       `json:"targets"`
}

type Image struct {
	Component string `json:"component"`
	Pinned    string `json:"pinned"`
}
type Target struct {
	Name         string  `json:"name"`
	Architecture string  `json:"architecture"`
	Images       []Image `json:"images"`
}

var username = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func Build(compute provision.Plan, lock release.Lock, access Access) (Plan, error) {
	if err := compute.Validate(); err != nil {
		return Plan{}, err
	}
	if err := lock.Validate(compute.Network); err != nil {
		return Plan{}, err
	}
	if !username.MatchString(access.User) || access.Port < 1 || access.Port > 65535 || (access.Address != "private" && access.Address != "public") {
		return Plan{}, errors.New("bootstrap requires an SSH user, port 1..65535, and private or public address selection")
	}
	if access.Address == "public" && !compute.Network.AWS.Provision.PublicIPv4 {
		return Plan{}, errors.New("compute plan has no public IPv4 addresses")
	}
	p := Plan{APIVersion: spec.Version, Kind: "NodeBootstrapPlan", Compute: compute, Release: lock, Access: access, RecipeSHA256: digest([]byte(recipe))}
	for _, node := range compute.Targets {
		t := Target{Name: node.Name, Architecture: node.Architecture}
		for _, image := range lock.Images {
			needed := image.Component == "core" || node.Role == "validator" || (node.Role == "seed" && image.Component == "tenderdash")
			if !needed {
				continue
			}
			repo, _ := name.NewDigest(image.Pinned, name.StrictValidation)
			for _, platform := range image.Platforms {
				if platform.Architecture == node.Architecture {
					t.Images = append(t.Images, Image{Component: image.Component, Pinned: repo.Context().Digest(platform.Digest).Name()})
				}
			}
		}
		sort.Slice(t.Images, func(i, j int) bool { return t.Images[i].Component < t.Images[j].Component })
		p.Targets = append(p.Targets, t)
	}
	b, _ := json.Marshal(p)
	p.ID = digest(b)
	return p, nil
}
func (p Plan) Validate() error {
	expected, err := Build(p.Compute, p.Release, p.Access)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(p, expected) {
		return errors.New("bootstrap plan altered or recipe differs from this CLI; use the original binary/plan, not an automatic recipe replacement")
	}
	return nil
}
func (p Plan) HostAlias(instanceID string) string {
	return fmt.Sprintf("%s.%s.%s.dashnet", instanceID, p.Compute.Network.AWS.Region, p.Compute.Network.AWS.AccountID)
}
