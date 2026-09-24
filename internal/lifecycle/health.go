package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/provision"
)

type HealthNode struct {
	Healthy        bool     `json:"healthy"`
	CoreHeight     int64    `json:"coreHeight"`
	PlatformHeight int64    `json:"platformHeight,omitempty"`
	Problems       []string `json:"problems"`
}
type Health struct {
	Network           string                `json:"network"`
	PlanID            string                `json:"planId"`
	ObservedAt        time.Time             `json:"observedAt"`
	Healthy           bool                  `json:"healthy"`
	Nodes             map[string]HealthNode `json:"nodes"`
	Problems          []string              `json:"problems"`
	ObservationWindow string                `json:"observationWindow"`
}
type samples struct {
	core     *node.Core
	platform *node.Platform
	problems []string
}

func (r Runner) sample(ctx context.Context, p Plan, record provision.Record, reference int64) map[string]samples {
	result := map[string]samples{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, t := range p.Targets {
		wg.Add(1)
		go func(t node.Target) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				result[t.Name] = samples{problems: []string{"observation deadline"}}
				mu.Unlock()
				return
			}
			defer func() { <-limit }()
			s := samples{}
			o, err := r.Remote.Call(ctx, runtimeRequest(p, record, t, "core-status"))
			if err != nil {
				s.problems = append(s.problems, "Core: "+node.SafeError(err))
			} else {
				s.core = o.Core
			}
			if t.Role == "validator" {
				q := runtimeRequest(p, record, t, "platform-status")
				q.ReferenceHeight = reference
				o, err = r.Remote.Call(ctx, q)
				if err != nil {
					s.problems = append(s.problems, "Platform: "+node.SafeError(err))
				} else {
					s.platform = o.Platform
				}
			}
			mu.Lock()
			result[t.Name] = s
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	return result
}

// Doctor makes two independent observations. No journal write, container command,
// restart, funding or configuration change is allowed through this path.
func (r Runner) Doctor(ctx context.Context, p Plan, record provision.Record) (Health, error) {
	h := Health{Network: p.Bootstrap.Compute.Network.Metadata.Name, PlanID: p.ID, Nodes: map[string]HealthNode{}, Problems: []string{}}
	window := r.ObservationWindow
	if window == 0 {
		window = 15 * time.Second
	}
	if window < 0 {
		return h, errors.New("health observation window must be positive")
	}
	h.ObservationWindow = window.String()
	if err := p.Validate(); err != nil {
		return h, err
	}
	if err := record.Validate(p.Bootstrap.Compute); err != nil {
		return h, err
	}
	if record.Deployment == nil || record.Deployment.PlanID != p.ID {
		return h, errors.New("missing matching deployment journal")
	}
	if _, err := effectiveImages(p, record); err != nil {
		return h, err
	}
	if r.Remote == nil {
		return h, errors.New("authenticated transport required")
	}
	if _, ok := ctx.Deadline(); !ok {
		return h, errors.New("doctor requires a deadline")
	}
	if err := provision.VerifyAccount(ctx, p.Bootstrap.Compute.Network.AWS.AccountID, r.Identity); err != nil {
		return h, err
	}
	if err := liveScope(ctx, p, record, r.Cloud); err != nil {
		return h, err
	}
	a := r.sample(ctx, p, record, 0)
	reference := int64(0)
	for _, t := range p.Validators() {
		if v := a[t.Name].platform; v != nil && v.Height > 0 && (reference == 0 || v.Height < reference) {
			reference = v.Height
		}
	}
	if r.Wait != nil {
		if err := r.Wait(ctx); err != nil {
			return h, err
		}
	} else {
		timer := time.NewTimer(window)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return h, ctx.Err()
		case <-timer.C:
		}
	}
	b := r.sample(ctx, p, record, reference)
	commonHash := ""
	for _, t := range p.Targets {
		v, first := b[t.Name], a[t.Name]
		n := HealthNode{Problems: append(first.problems, v.problems...)}
		if err := coreHealthy(t, record.Deployment.Nodes[t.Name], v.core, p.Miner().Name); err != nil {
			n.Problems = append(n.Problems, err.Error())
		}
		if v.core != nil {
			n.CoreHeight = v.core.Height
			if v.core.Genesis != record.Deployment.CoreGenesis {
				n.Problems = append(n.Problems, "Core genesis mismatch")
			}
			if first.core == nil || v.core.Height <= first.core.Height {
				n.Problems = append(n.Problems, "Core did not advance")
			}
			if first.core != nil && first.core.Mining != nil && v.core.Mining != nil && (first.core.Mining.ContainerID != v.core.Mining.ContainerID || first.core.Mining.Restarts != v.core.Mining.Restarts) {
				n.Problems = append(n.Problems, "miner restarted during observation")
			}
			if first.core != nil && (first.core.ContainerID != v.core.ContainerID || first.core.Restarts != v.core.Restarts) {
				n.Problems = append(n.Problems, "Core container changed during observation")
			}
		}
		if t.Role == "validator" {
			expected := record.Deployment.Nodes[t.Name]
			if v.platform == nil {
				n.Problems = append(n.Problems, "Platform unavailable")
			} else {
				x := v.platform
				n.PlatformHeight = x.Height
				if x.CatchingUp || x.Height < 1 || x.DAPIHeight < 1 || x.DAPIHeight < x.Height-3 || x.DriveVersion == "" || x.ChainID != p.PlatformChainID || x.NodeID != expected.PlatformNodeID || !strings.EqualFold(x.ProTxHash, expected.ProTxHash) {
					n.Problems = append(n.Problems, "Platform/DAPI identity or sync mismatch")
				}
				if first.platform == nil || x.Height <= first.platform.Height {
					n.Problems = append(n.Problems, "Platform did not advance")
				}
				if len(x.ReferenceBlockHash) != 64 {
					n.Problems = append(n.Problems, "missing common-height block hash")
				} else if commonHash == "" {
					commonHash = x.ReferenceBlockHash
				} else if commonHash != x.ReferenceBlockHash {
					n.Problems = append(n.Problems, "Platform block hash disagreement")
				}
				if len(x.Containers) != 4 {
					n.Problems = append(n.Problems, "incomplete Platform service set")
				}
				if first.platform != nil {
					for name, id := range first.platform.Containers {
						if x.Containers[name] != id || x.Restarts[name] != first.platform.Restarts[name] {
							n.Problems = append(n.Problems, "Platform container changed: "+name)
						}
					}
				}
			}
		}
		n.Healthy = len(n.Problems) == 0
		h.Nodes[t.Name] = n
		if !n.Healthy {
			h.Problems = append(h.Problems, fmt.Sprintf("%s: %s", t.Name, strings.Join(n.Problems, ", ")))
		}
	}
	// Re-check cloud identity after the observation window, not only before it.
	if err := liveScope(ctx, p, record, r.Cloud); err != nil {
		return h, err
	}
	h.ObservedAt = time.Now().UTC()
	h.Healthy = len(h.Problems) == 0
	return h, nil
}
