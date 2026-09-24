package provision

import (
	"errors"
	"time"
)

// JoinProgress tracks an independent, owned allocation joining an existing chain.
// It never adopts or rewrites the source fleet's operation record.
type JoinProgress struct {
	PlanID string              `json:"planId"`
	Phase  string              `json:"phase"`
	Nodes  map[string]JoinNode `json:"nodes"`
}
type JoinNode struct {
	Phase       string    `json:"phase"`
	ContainerID string    `json:"containerId,omitempty"`
	Height      int64     `json:"height"`
	ObservedAt  time.Time `json:"observedAt,omitempty"`
}

func (r Record) validateJoin(p Plan) error {
	j := r.Join
	if j == nil {
		return nil
	}
	if r.Deployment != nil || r.Runtime != nil || r.Upgrade != nil || !hex64.MatchString(j.PlanID) || len(j.Nodes) != len(p.Targets) {
		return errors.New("join record conflicts with a genesis deployment or lost targets")
	}
	if j.Phase != "joining" && j.Phase != "interrupted" && j.Phase != "joined" {
		return errors.New("invalid join phase")
	}
	for _, t := range p.Targets {
		n, ok := j.Nodes[t.Name]
		if !ok || t.Role != "fullnode" || (n.Phase != "pending" && n.Phase != "syncing" && n.Phase != "ready") || n.Height < 0 || (n.ContainerID != "" && !hex64.MatchString(n.ContainerID)) {
			return errors.New("invalid joined target")
		}
		if j.Phase == "joined" && (n.Phase != "ready" || n.ObservedAt.IsZero() || n.ContainerID == "") {
			return errors.New("join ready without observed evidence")
		}
	}
	return nil
}
