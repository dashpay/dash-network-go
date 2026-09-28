package lifecycle

import (
	"errors"
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
	return q
}
