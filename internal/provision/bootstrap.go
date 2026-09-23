package provision

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Bootstrap is an additive, immutable preparation stage in the existing network
// journal. Old compute-only records remain readable. No lifecycle rotation yet.
type BootstrapProgress struct {
	PlanID string                   `json:"planId"`
	Phase  string                   `json:"phase"`
	Nodes  map[string]BootstrapNode `json:"nodes"`
}

type BootstrapNode struct {
	Phase          string    `json:"phase"`
	ObservedAt     time.Time `json:"observedAt,omitempty"`
	DockerVersion  string    `json:"dockerVersion,omitempty"`
	ComposeVersion string    `json:"composeVersion,omitempty"`
}

var bootstrapID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var runtimeVersion = regexp.MustCompile(`^[a-zA-Z0-9.+:~_-]{1,100}$`)

func (r Record) validateBootstrap(p Plan) error {
	b := r.Bootstrap
	if b == nil {
		return nil
	}
	if !bootstrapID.MatchString(b.PlanID) || len(b.Nodes) != len(p.Targets) {
		return errors.New("invalid bootstrap identity or target set")
	}
	switch b.Phase {
	case "preparing", "interrupted", "hosts-ready":
	default:
		return errors.New("invalid bootstrap phase")
	}
	for _, t := range p.Targets {
		n, ok := b.Nodes[t.Name]
		if !ok {
			return fmt.Errorf("bootstrap lost target %s", t.Name)
		}
		switch n.Phase {
		case "pending", "checking", "preparing", "unknown":
		case "ready":
			if n.ObservedAt.IsZero() || !runtimeVersion.MatchString(n.DockerVersion) || !runtimeVersion.MatchString(n.ComposeVersion) {
				return errors.New("missing bootstrap runtime evidence")
			}
		default:
			return errors.New("invalid bootstrap target phase")
		}
		if b.Phase == "hosts-ready" && n.Phase != "ready" {
			return errors.New("hosts-ready requires every target verified")
		}
	}
	return nil
}

// RunningTargets reuses the provisioner's full ownership/placement checks and
// binds EVERY target to the existing journal before any SSH operation.
func RunningTargets(ctx context.Context, p Plan, r Record, cloud EC2) (map[string]types.Instance, error) {
	if err := r.Validate(p); err != nil {
		return nil, err
	}
	if r.Phase != "compute-ready" {
		return nil, errors.New("bootstrap requires completed EC2 provisioning")
	}
	live, err := discover(ctx, p, cloud)
	if err != nil {
		return nil, err
	}
	for _, t := range p.Targets {
		i, ok := live[t.Name]
		n := r.Nodes[t.Name]
		if !ok || n.Phase != "present" || aws.ToString(i.InstanceId) != n.InstanceID || i.State.Name != types.InstanceStateNameRunning {
			return nil, fmt.Errorf("target %s is missing, changed, or not running", t.Name)
		}
	}
	return live, nil
}
