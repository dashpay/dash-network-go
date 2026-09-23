// Package release resolves mutable image references once per network operation.
package release

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/dashpay/dash-network-go/internal/spec"
	"github.com/google/go-containerregistry/pkg/name"
)

const Evidence = "artifacts-verified"

type Lock struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Network    string    `json:"network"`
	Generation int       `json:"generation"`
	SpecHash   string    `json:"specHash"`
	ResolvedAt time.Time `json:"resolvedAt"`
	Evidence   string    `json:"evidence"`
	Images     []Image   `json:"images"`
}

type Image struct {
	Component string     `json:"component"`
	Requested string     `json:"requested"`
	Pinned    string     `json:"pinned"`
	Platforms []Platform `json:"platforms"`
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Digest       string `json:"digest"`
}

type Inspector interface {
	Inspect(context.Context, string, []string) (Image, error)
}

func Resolve(ctx context.Context, n spec.Network, inspector Inspector, now time.Time) (Lock, error) {
	if err := n.Validate(); err != nil {
		return Lock{}, err
	}
	lock := Lock{APIVersion: spec.Version, Kind: "ReleaseLock", Network: n.Metadata.Name,
		Generation: n.Chain.Generation, SpecHash: n.Fingerprint(), ResolvedAt: now.UTC(), Evidence: Evidence}
	for _, component := range spec.Components {
		image, err := inspector.Inspect(ctx, n.Images[component], n.Architectures())
		if err != nil {
			return Lock{}, fmt.Errorf("resolve %s: %w", component, err)
		}
		image.Component = component
		lock.Images = append(lock.Images, image)
	}
	return lock, lock.Validate(n)
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (l Lock) Validate(n spec.Network) error {
	if l.APIVersion != spec.Version || l.Kind != "ReleaseLock" || l.Evidence != Evidence || l.ResolvedAt.IsZero() {
		return errors.New("invalid release lock version, kind, evidence, or timestamp")
	}
	if l.Network != n.Metadata.Name || l.Generation != n.Chain.Generation || l.SpecHash != n.Fingerprint() {
		return errors.New("release lock does not match this network definition/generation; resolve it again")
	}
	if len(l.Images) != len(n.Images) {
		return errors.New("release lock component set is incomplete")
	}
	seen := map[string]bool{}
	for _, image := range l.Images {
		requested, exists := n.Images[image.Component]
		if !exists || seen[image.Component] || image.Requested != requested {
			return fmt.Errorf("release lock component %q is duplicated or differs from intent", image.Component)
		}
		seen[image.Component] = true
		ref, err := name.ParseReference(requested, name.StrictValidation)
		if err != nil {
			return err
		}
		pinned, err := name.NewDigest(image.Pinned, name.StrictValidation)
		if err != nil || !digestPattern.MatchString(pinned.DigestStr()) || ref.Context().Name() != pinned.Context().Name() {
			return fmt.Errorf("release lock component %s must pin the requested repository by sha256", image.Component)
		}
		if original, ok := ref.(name.Digest); ok && original.DigestStr() != pinned.DigestStr() {
			return fmt.Errorf("release lock component %s changed an explicitly requested digest", image.Component)
		}
		arches := n.Architectures()
		if len(image.Platforms) != len(arches) {
			return fmt.Errorf("%s platform set differs from network architectures", image.Component)
		}
		got := make([]string, 0, len(image.Platforms))
		for _, p := range image.Platforms {
			if p.OS != "linux" || !digestPattern.MatchString(p.Digest) {
				return fmt.Errorf("invalid platform descriptor for %s", image.Component)
			}
			got = append(got, p.Architecture)
		}
		sort.Strings(got)
		for i := range arches {
			if arches[i] != got[i] {
				return fmt.Errorf("missing/duplicate architecture for %s", image.Component)
			}
		}
	}
	return nil
}
