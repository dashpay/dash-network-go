package provision

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// SidecarImage pins an image the release's dashmate selects for a service no
// release component names (Tor, the gateway rate limiter and its Redis). The
// requested reference is dashmate's own; the pin is resolved once and reused.
type SidecarImage struct {
	Service   string            `json:"service"`
	Requested string            `json:"requested"`
	Pinned    string            `json:"pinned"`
	Platforms []SidecarPlatform `json:"platforms"`
}

type SidecarPlatform struct {
	Architecture string `json:"architecture"`
	Digest       string `json:"digest"`
}

type Sidecars []SidecarImage

// SidecarServices are the dashmate services whose images the controller pins.
var SidecarServices = []string{"core_tor", "gateway_rate_limiter", "gateway_rate_limiter_redis"}

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Reference parses an image reference as Docker does, as dashmate writes
// them: Docker Hub by default ("redis:alpine"), and the tag-and-digest form
// ("repo:tag@sha256:..."), in which the digest decides.
func Reference(ref string) (name.Reference, error) {
	if repo, digest, ok := strings.Cut(ref, "@"); ok {
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			if _, err := name.NewTag(repo); err != nil {
				return nil, err
			}
			repo = repo[:i]
		}
		return name.NewDigest(repo + "@" + digest)
	}
	return name.ParseReference(ref)
}

func (s Sidecars) Validate(p Plan) error {
	arches := p.Network.Architectures()
	seen := map[string]bool{}
	for i, image := range s {
		if !slices.Contains(SidecarServices, image.Service) || seen[image.Service] || (i > 0 && s[i-1].Service > image.Service) {
			return errors.New("sidecar images must name distinct supported dashmate services in order")
		}
		seen[image.Service] = true
		requested, err := Reference(image.Requested)
		if err != nil {
			return fmt.Errorf("invalid requested %s image", image.Service)
		}
		pinned, err := name.NewDigest(image.Pinned, name.StrictValidation)
		if err != nil || !sha256Digest.MatchString(pinned.DigestStr()) || pinned.Context().Name() != requested.Context().Name() {
			return fmt.Errorf("%s must pin dashmate's requested repository by sha256", image.Service)
		}
		if digest, ok := requested.(name.Digest); ok && digest.DigestStr() != pinned.DigestStr() {
			return fmt.Errorf("%s pin differs from dashmate's own digest", image.Service)
		}
		var got []string
		for _, platform := range image.Platforms {
			if !sha256Digest.MatchString(platform.Digest) {
				return fmt.Errorf("invalid %s platform digest", image.Service)
			}
			got = append(got, platform.Architecture)
		}
		sort.Strings(got)
		if !slices.Equal(got, arches) {
			return fmt.Errorf("%s platforms differ from network architectures", image.Service)
		}
	}
	return nil
}

// For returns each service's architecture-specific pinned reference.
func (s Sidecars) For(architecture string) map[string]string {
	out := map[string]string{}
	for _, image := range s {
		repo, err := name.NewDigest(image.Pinned, name.StrictValidation)
		if err != nil {
			continue
		}
		for _, platform := range image.Platforms {
			if platform.Architecture == architecture {
				out[image.Service] = repo.Context().Digest(platform.Digest).Name()
			}
		}
	}
	return out
}

// Requested maps each pinned service to dashmate's requested reference.
func (s Sidecars) Requested() map[string]string {
	out := map[string]string{}
	for _, image := range s {
		out[image.Service] = image.Requested
	}
	return out
}

// Target finds a planned target by name.
func (p Plan) Target(name string) (Target, bool) {
	for _, t := range p.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}
