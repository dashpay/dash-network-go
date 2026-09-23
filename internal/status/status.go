// Package status defines an explicit, narrow public projection. Never serialize
// an operator inventory and try to hide its fields in the browser.
package status

import (
	"errors"
	"time"

	"github.com/dashpay/dash-network-go/internal/inventory"
	"github.com/dashpay/dash-network-go/internal/spec"
)

type Public struct {
	APIVersion        string         `json:"apiVersion"`
	Kind              string         `json:"kind"`
	Name              string         `json:"name"`
	DisplayName       string         `json:"displayName"`
	Description       string         `json:"description"`
	NetworkType       string         `json:"networkType"`
	Generation        int            `json:"generation"`
	ObservedAt        time.Time      `json:"observedAt"`
	Stale             bool           `json:"stale"`
	ObservationSource string         `json:"observationSource"`
	ApplicationHealth string         `json:"applicationHealth"`
	InstanceCount     int            `json:"instanceCount"`
	Infrastructure    map[string]int `json:"infrastructure"`
}

func Project(s inventory.Snapshot, now time.Time, maxAge time.Duration) (Public, error) {
	if err := s.Validate(); err != nil {
		return Public{}, err
	}
	if s.Network.Visibility != "public" {
		return Public{}, errors.New("private network: public export is disabled")
	}
	if maxAge <= 0 {
		return Public{}, errors.New("max age must be positive")
	}
	age := now.Sub(s.ObservedAt)
	p := Public{APIVersion: spec.Version, Kind: "PublicNetworkStatus", Name: s.Network.Name,
		DisplayName: s.Network.DisplayName, Description: s.Network.Description, NetworkType: s.Chain.Type,
		Generation: s.Chain.Generation, ObservedAt: s.ObservedAt, Stale: age > maxAge || age < -time.Minute,
		ObservationSource: s.Source, ApplicationHealth: "unknown", InstanceCount: len(s.Instances), Infrastructure: map[string]int{}}
	for _, i := range s.Instances {
		switch i.State {
		case "pending", "running", "stopping", "stopped", "shutting-down", "terminated":
			p.Infrastructure[i.State]++
		default:
			p.Infrastructure["unknown"]++
		}
	}
	return p, nil
}
