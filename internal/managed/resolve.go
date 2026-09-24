package managed

import (
	"context"
	"errors"
	"fmt"
	"github.com/dashpay/dash-network-go/internal/release"
	"github.com/google/go-containerregistry/pkg/name"
	"slices"
)

// Resolve captures each reference once per architecture, never once per node.
func Resolve(ctx context.Context, s Snapshot, scope string, choices map[string]string, registry release.Inspector) (Images, error) {
	if err := s.Complete(); err != nil {
		return nil, err
	}
	for c := range choices {
		if !slices.Contains(Components, c) || !Selected(scope, c) {
			return nil, errors.New("image override outside selected scope")
		}
	}
	result := Images{}
	cache := map[string]string{}
	for _, t := range s.Fleet.Targets {
		result[t.Name] = map[string]string{}
		for c := range t.Containers {
			if !Selected(scope, c) {
				continue
			}
			ref := choices[c]
			if ref == "" {
				ds := s.Nodes[t.Name].Components[c].Digests
				if len(ds) == 0 {
					return nil, fmt.Errorf("%s/%s has no immutable source; declare a candidate", t.Name, c)
				}
				ref = ds[0]
			}
			key := ref + "/" + t.Architecture
			pin := cache[key]
			if pin == "" {
				image, e := registry.Inspect(ctx, ref, []string{t.Architecture})
				if e != nil {
					return nil, e
				}
				if len(image.Platforms) != 1 {
					return nil, errors.New("image architecture resolution incomplete")
				}
				d, e := name.NewDigest(image.Pinned)
				if e != nil {
					return nil, e
				}
				pin = d.Context().Digest(image.Platforms[0].Digest).Name()
				cache[key] = pin
			}
			result[t.Name][c] = pin
		}
	}
	return result, nil
}
