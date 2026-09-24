package provision

import (
	"errors"
	"regexp"
	"time"
)

// DeploymentProgress contains public operational facts, never RPC credentials,
// wallet keys, validator keys, signed transactions or arbitrary command output.
type DeploymentProgress struct {
	PlanID            string                    `json:"planId"`
	Phase             string                    `json:"phase"`
	Stage             string                    `json:"stage"`
	CoreGenesis       string                    `json:"coreGenesis,omitempty"`
	PayoutAddress     string                    `json:"payoutAddress,omitempty"`
	SporkAddress      string                    `json:"sporkAddress,omitempty"`
	GenesisCoreHeight int64                     `json:"genesisCoreHeight,omitempty"`
	ObservedAt        time.Time                 `json:"observedAt,omitempty"`
	Nodes             map[string]DeploymentNode `json:"nodes"`
}

type DeploymentNode struct {
	Phase             string    `json:"phase"`
	OperatorPublicKey string    `json:"operatorPublicKey,omitempty"`
	PlatformNodeID    string    `json:"platformNodeId,omitempty"`
	ProTxHash         string    `json:"proTxHash,omitempty"`
	CoreContainerID   string    `json:"coreContainerId,omitempty"`
	CoreHeight        int64     `json:"coreHeight,omitempty"`
	PlatformHeight    int64     `json:"platformHeight,omitempty"`
	ObservedAt        time.Time `json:"observedAt,omitempty"`
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
var hex96 = regexp.MustCompile(`^[0-9a-f]{96}$`)
var dashAddress = regexp.MustCompile(`^[123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz]{26,40}$`)

func (r Record) validateDeployment(p Plan) error {
	d := r.Deployment
	if d == nil {
		return nil
	}
	if !hex64.MatchString(d.PlanID) || len(d.Nodes) != len(p.Targets) || d.GenesisCoreHeight < 0 {
		return errors.New("invalid deployment journal identity or scope")
	}
	switch d.Phase {
	case "deploying", "interrupted", "network-ready", "stopped":
	default:
		return errors.New("invalid deployment phase")
	}
	switch d.Stage {
	case "preflight", "core-start", "identities", "core-finalize", "registrations", "quorums", "platform-start", "health", "ready", "stopping", "stopped":
	default:
		return errors.New("invalid deployment stage")
	}
	if d.CoreGenesis != "" && !hex64.MatchString(d.CoreGenesis) {
		return errors.New("invalid Core genesis evidence")
	}
	for _, a := range []string{d.PayoutAddress, d.SporkAddress} {
		if a != "" && !dashAddress.MatchString(a) {
			return errors.New("invalid public wallet address")
		}
	}
	for _, t := range p.Targets {
		n, ok := d.Nodes[t.Name]
		if !ok {
			return errors.New("deployment lost an intended target")
		}
		switch n.Phase {
		case "pending", "checking", "core-ready", "identified", "registered", "platform-started", "ready", "unknown":
		default:
			return errors.New("invalid deployment target phase")
		}
		if n.OperatorPublicKey != "" && !hex96.MatchString(n.OperatorPublicKey) || n.PlatformNodeID != "" && !hex40.MatchString(n.PlatformNodeID) || n.ProTxHash != "" && !hex64.MatchString(n.ProTxHash) || n.CoreContainerID != "" && !hex64.MatchString(n.CoreContainerID) || n.CoreHeight < 0 || n.PlatformHeight < 0 {
			return errors.New("invalid deployment target evidence")
		}
		if d.Phase == "network-ready" && (n.Phase != "ready" || n.ObservedAt.IsZero()) {
			return errors.New("network readiness requires all targets observed")
		}
	}
	return nil
}
