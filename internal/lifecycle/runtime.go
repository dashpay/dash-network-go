package lifecycle

import (
	"errors"
	"sort"

	"github.com/dashpay/dash-network-go/internal/bootstrap"
	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
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
	for name, components := range images {
		if record.Runtime.Images[name]["core"] != components["core"] {
			return nil, errors.New("runtime attempted to replace preserved Core")
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
