package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
	"github.com/google/go-containerregistry/pkg/name"
)

func cloneImages(images provision.FleetImages) provision.FleetImages {
	out := provision.FleetImages{}
	for name, components := range images {
		out[name] = provision.ImageSet{}
		for component, pin := range components {
			out[name][component] = pin
		}
	}
	return out
}

func effectiveImages(p Plan, record provision.Record) (provision.FleetImages, error) {
	images := provision.FleetImages{}
	for _, t := range p.Targets {
		images[t.Name] = provision.ImageSet{}
		for _, image := range t.Images {
			images[t.Name][image.Component] = image.Pinned
		}
	}
	if record.Runtime == nil {
		return images, nil
	}
	if record.Runtime.DeploymentID != p.ID {
		return nil, errors.New("runtime belongs to another deployment")
	}
	if err := record.Runtime.Images.Validate(p.Bootstrap.Compute); err != nil {
		return nil, err
	}
	// Core may differ from the deployment plan only through reviewed Core
	// upgrades (the journal bounds runtime images by each upgrade's From/To,
	// chained by previous ID); it always stays the same repository.
	for n, components := range images {
		was, errWas := name.NewDigest(components["core"], name.StrictValidation)
		now, errNow := name.NewDigest(record.Runtime.Images[n]["core"], name.StrictValidation)
		if errWas != nil || errNow != nil || was.Context().Name() != now.Context().Name() {
			return nil, errors.New("runtime attempted to replace Core with another repository")
		}
	}
	return cloneImages(record.Runtime.Images), nil
}

func runtimeRequest(p Plan, record provision.Record, t node.Target, action string) node.Request {
	q := p.Request(t, action)
	if record.Runtime != nil {
		q.Target.Images = nil
		for component, pin := range record.Runtime.Images[t.Name] {
			q.Target.Images = append(q.Target.Images, bootstrap.Image{Component: component, Pinned: pin})
		}
		sort.Slice(q.Target.Images, func(i, j int) bool { return q.Target.Images[i].Component < q.Target.Images[j].Component })
	}
	if pins := effectiveSidecars(record).For(t.Architecture); len(pins) > 0 {
		q.Context.SidecarImages = pins
	}
	return q
}

// effectiveSidecars are the sidecar pins the fleet currently runs: the last
// completed upgrade's, else the deployment's.
func effectiveSidecars(record provision.Record) provision.Sidecars {
	if record.Runtime != nil && len(record.Runtime.Sidecars) > 0 {
		return record.Runtime.Sidecars
	}
	if record.Deployment != nil {
		return record.Deployment.Sidecars
	}
	return nil
}

// pinSidecars resolves each sidecar image dashmate requests, keeping pins
// already resolved for the same request. Registry access is anonymous.
func (r Runner) pinSidecars(ctx context.Context, p Plan, current provision.Sidecars, requested map[string]string) (provision.Sidecars, error) {
	known := map[string]provision.SidecarImage{}
	for _, image := range current {
		known[image.Service] = image
	}
	var out provision.Sidecars
	services := make([]string, 0, len(requested))
	for service := range requested {
		services = append(services, service)
	}
	sort.Strings(services)
	for _, service := range services {
		ref := requested[service]
		if image, ok := known[service]; ok && image.Requested == ref {
			out = append(out, image)
			continue
		}
		if r.Registry == nil {
			return nil, errors.New("pinning dashmate's sidecar images needs registry access")
		}
		parsed, err := provision.Reference(ref)
		if err != nil {
			return nil, fmt.Errorf("invalid %s image %s", service, ref)
		}
		resolved, err := r.Registry.Inspect(ctx, parsed.Name(), p.Bootstrap.Compute.Network.Architectures())
		if err != nil {
			return nil, fmt.Errorf("pin %s image %s: %w", service, ref, err)
		}
		image := provision.SidecarImage{Service: service, Requested: ref, Pinned: resolved.Pinned}
		for _, platform := range resolved.Platforms {
			image.Platforms = append(image.Platforms, provision.SidecarPlatform{Architecture: platform.Architecture, Digest: platform.Digest})
		}
		out = append(out, image)
	}
	return out, out.Validate(p.Bootstrap.Compute)
}

// renders collects one dashmate render per target: every node must render
// with the same release and request the same sidecar images.
func renders(observed map[string]*node.Render) (string, map[string]string, error) {
	version, requested := "", map[string]string{}
	names := make([]string, 0, len(observed))
	for name := range observed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := observed[name]
		if r == nil {
			return "", nil, fmt.Errorf("%s returned no dashmate render", name)
		}
		if version == "" {
			version = r.Version
		} else if r.Version != version {
			return "", nil, fmt.Errorf("%s renders with dashmate %s, other nodes with %s", name, r.Version, version)
		}
		for service, ref := range r.Sidecars {
			if previous, ok := requested[service]; ok && previous != ref {
				return "", nil, fmt.Errorf("nodes request different %s images", service)
			}
			requested[service] = ref
		}
	}
	return version, requested, nil
}
