package release

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Registry uses anonymous registry access. It never reads a Docker credential
// helper or sends operator credentials to a registry named in a network file.
type Registry struct{ Transport http.RoundTripper }

func (r Registry) Inspect(ctx context.Context, requested string, arches []string) (Image, error) {
	ref, err := name.ParseReference(requested, name.StrictValidation)
	if err != nil {
		return Image{}, err
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.Anonymous), remote.WithUserAgent("dash-network-go")}
	if r.Transport != nil {
		opts = append(opts, remote.WithTransport(r.Transport))
	}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return Image{}, fmt.Errorf("read image manifest: %w", err)
	}
	image := Image{Requested: requested, Pinned: ref.Context().Digest(desc.Digest.String()).Name()}
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return Image{}, err
		}
		manifest, err := idx.IndexManifest()
		if err != nil {
			return Image{}, err
		}
		for _, arch := range arches {
			var found []v1.Descriptor
			for _, child := range manifest.Manifests {
				if child.Platform != nil && child.Platform.OS == "linux" && child.Platform.Architecture == arch &&
					(child.Platform.Variant == "" || (arch == "arm64" && child.Platform.Variant == "v8")) {
					found = append(found, child)
				}
			}
			if len(found) != 1 {
				return Image{}, fmt.Errorf("expected one linux/%s image, found %d", arch, len(found))
			}
			// Check the child manifest and its actual configuration, not just the
			// index's advertised platform. No image layers are downloaded.
			child, err := idx.Image(found[0].Digest)
			if err != nil {
				return Image{}, err
			}
			cfg, err := child.ConfigFile()
			if err != nil {
				return Image{}, err
			}
			if cfg.OS != "linux" || cfg.Architecture != arch {
				return Image{}, fmt.Errorf("image configuration disagrees with linux/%s descriptor", arch)
			}
			image.Platforms = append(image.Platforms, Platform{OS: "linux", Architecture: arch, Digest: found[0].Digest.String()})
		}
	} else {
		child, err := desc.Image()
		if err != nil {
			return Image{}, err
		}
		cfg, err := child.ConfigFile()
		if err != nil {
			return Image{}, err
		}
		if len(arches) != 1 || cfg.OS != "linux" || cfg.Architecture != arches[0] {
			return Image{}, fmt.Errorf("single-platform image is %s/%s, network requires linux/%v", cfg.OS, cfg.Architecture, arches)
		}
		image.Platforms = []Platform{{OS: "linux", Architecture: cfg.Architecture, Digest: desc.Digest.String()}}
	}
	sort.Slice(image.Platforms, func(i, j int) bool { return image.Platforms[i].Architecture < image.Platforms[j].Architecture })
	return image, nil
}
