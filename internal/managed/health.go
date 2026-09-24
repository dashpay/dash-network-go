package managed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dashpay/dash-network-go/internal/node"
	"github.com/dashpay/dash-network-go/internal/spec"
)

func request(f Fleet, t Target, action string) Request {
	return Request{Fleet: f, FleetID: f.ID(), Target: t, Action: action}
}
func Observe(ctx context.Context, f Fleet, remote Backend, reference int64) Snapshot {
	result := Snapshot{APIVersion: spec.Version, Kind: "ExistingSnapshot", Fleet: f, ObservedAt: time.Now().UTC(), Nodes: map[string]Observation{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, t := range f.Targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			var o Observation
			var err error
			select {
			case limit <- struct{}{}:
				q := request(f, t, "observe")
				q.ReferenceHeight = reference
				o, err = remote.Call(ctx, q)
				<-limit
			case <-ctx.Done():
				err = ctx.Err()
			}
			if err != nil {
				o = Observation{InstanceID: t.InstanceID, At: time.Now().UTC(), Error: node.SafeError(err)}
			}
			mu.Lock()
			result.Nodes[t.Name] = o
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	result.ID = hash(result)
	return result
}

type Health struct {
	Healthy    bool      `json:"healthy"`
	ObservedAt time.Time `json:"observedAt"`
	Window     string    `json:"window"`
	Problems   []string  `json:"problems"`
	Snapshot   Snapshot  `json:"snapshot"`
}

func (o Observation) Healthy(t Target) error {
	if o.Error != "" || len(o.Problems) > 0 {
		return errors.New("observation incomplete")
	}
	for _, c := range o.Components {
		if !c.Running {
			return errors.New("service not running")
		}
	}
	if t.Containers["core"] != "" && (!o.Chain.CoreSynced || o.Chain.ChainLockHeight <= 0) {
		return errors.New("Core not synced/ChainLocked")
	}
	if t.Role == "validator" && (o.Chain.MasternodeState != "READY" || o.Chain.CatchingUp || !o.Chain.DAPIHealthy || o.Chain.PlatformHeight < 1 || o.Chain.DAPIHeight < 1) {
		return errors.New("validator/consensus/DAPI unhealthy")
	}
	return nil
}
func SameIdentity(old, new Observation, t Target) bool {
	if old.FilesHash != new.FilesHash {
		return false
	}
	if t.Containers["core"] != "" && (old.Chain.CoreGenesis != new.Chain.CoreGenesis || old.Chain.CoreNetwork != new.Chain.CoreNetwork) {
		return false
	}
	if t.Role == "validator" && (old.Chain.PlatformChainID != new.Chain.PlatformChainID || old.Chain.PlatformNodeID != new.Chain.PlatformNodeID || old.Chain.ProTxHash != new.Chain.ProTxHash || old.Chain.PlatformProtocol != new.Chain.PlatformProtocol) {
		return false
	}
	return true
}
func Doctor(ctx context.Context, s Snapshot, remote Backend, window time.Duration, wait func(context.Context, time.Duration) error) (Health, error) {
	h := Health{Window: window.String(), Problems: []string{}}
	if err := s.Complete(); err != nil {
		return h, err
	}
	if window <= 0 {
		return h, errors.New("positive observation window required")
	}
	first := Observe(ctx, s.Fleet, remote, 0)
	reference := int64(0)
	for _, t := range s.Fleet.Targets {
		if t.Role == "validator" {
			n := first.Nodes[t.Name].Chain.PlatformHeight
			if n > 0 && (reference == 0 || n < reference) {
				reference = n
			}
		}
	}
	if err := wait(ctx, window); err != nil {
		return h, err
	}
	second := Observe(ctx, s.Fleet, remote, reference)
	h.Snapshot = second
	block := ""
	for _, t := range s.Fleet.Targets {
		a, b := first.Nodes[t.Name], second.Nodes[t.Name]
		if err := a.Healthy(t); err != nil {
			h.Problems = append(h.Problems, t.Name+": first "+err.Error())
		}
		if err := b.Healthy(t); err != nil {
			h.Problems = append(h.Problems, t.Name+": "+err.Error())
		}
		if !SameIdentity(s.Nodes[t.Name], b, t) {
			h.Problems = append(h.Problems, t.Name+": identity/configuration/protocol changed")
		}
		if t.Containers["core"] != "" && (b.Chain.CoreHeight <= a.Chain.CoreHeight || b.Chain.ChainLockHeight < a.Chain.ChainLockHeight) {
			h.Problems = append(h.Problems, t.Name+": Core not advancing")
		}
		if t.Role == "validator" {
			if b.Chain.PlatformHeight <= a.Chain.PlatformHeight || b.Chain.DAPIHeight < a.Chain.DAPIHeight || b.Chain.DAPIHeight > b.Chain.PlatformHeight+5 || b.Chain.DAPIHeight+5 < b.Chain.PlatformHeight {
				h.Problems = append(h.Problems, t.Name+": Platform not advancing")
			}
			if !digestRE.MatchString(b.Chain.PlatformHash) || (block != "" && block != b.Chain.PlatformHash) {
				h.Problems = append(h.Problems, t.Name+": consensus block disagreement")
			}
			block = b.Chain.PlatformHash
		}
	}
	h.Healthy = len(h.Problems) == 0
	h.ObservedAt = time.Now().UTC()
	return h, nil
}

// Count only observed healthy owned validators, not guessed availability of
// unrelated public-testnet operators. Every observed quorum must retain >2/3.
func CanWithdraw(s Snapshot, target Target) error {
	if target.Role != "validator" {
		return nil
	}
	healthy := map[string]bool{}
	for _, t := range s.Fleet.Targets {
		if t.Role == "validator" && t.Name != target.Name && s.Nodes[t.Name].Healthy(t) == nil {
			healthy[strings.ToLower(s.Nodes[t.Name].Chain.ProTxHash)] = true
		}
	}
	for _, t := range s.Fleet.Targets {
		if t.Role != "validator" {
			continue
		}
		powers := s.Nodes[t.Name].Chain.VotingPower
		if len(powers) == 0 {
			return errors.New("quorum membership unavailable")
		}
		total, remaining := int64(0), int64(0)
		for id, power := range powers {
			if !digestRE.MatchString(id) || power <= 0 || power > 1000000000 {
				return errors.New("invalid quorum power")
			}
			total += power
			if healthy[id] {
				remaining += power
			}
		}
		if remaining <= 2*total/3 {
			return fmt.Errorf("withdrawing %s lacks observed >2/3 voting power in quorum reported by %s", target.Name, t.Name)
		}
	}
	return nil
}
func nativePreserved(base, current map[string]string, selected []string) bool {
	filter := func(v map[string]string) map[string]string {
		out := map[string]string{}
		for k, s := range v {
			if slices.Contains(selected, "core") && strings.HasPrefix(k, "dashd:") {
				continue
			}
			out[k] = s
		}
		return out
	}
	return hash(filter(base)) == hash(filter(current))
}
func Preserved(base, current Observation, selected []string) error {
	if base.FilesHash != current.FilesHash || hash(base.Companions) != hash(current.Companions) || !nativePreserved(base.NativeProcesses, current.NativeProcesses, selected) {
		return errors.New("configuration/companion drift")
	}
	for c, b := range base.Components {
		a, ok := current.Components[c]
		if !ok || a.ConfigHash != b.ConfigHash {
			return errors.New("workload configuration drift")
		}
		if !slices.Contains(selected, c) && hash(a) != hash(b) {
			return fmt.Errorf("unselected %s changed", c)
		}
	}
	return nil
}
