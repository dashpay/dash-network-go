package provision

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/dashpay/dash-network-go/internal/inventory"
)

type NodeProgress struct {
	Phase       string    `json:"phase"` // pending, launching, present
	AttemptedAt time.Time `json:"attemptedAt,omitempty"`
	InstanceID  string    `json:"instanceId,omitempty"`
	EC2State    string    `json:"ec2State"`
	ObservedAt  time.Time `json:"observedAt,omitempty"`
}

type Record struct {
	APIVersion        string                  `json:"apiVersion"`
	Kind              string                  `json:"kind"`
	Revision          int64                   `json:"revision"`
	Plan              Plan                    `json:"plan"`
	Phase             string                  `json:"phase"`
	ApplicationHealth string                  `json:"applicationHealth"`
	CreatedAt         time.Time               `json:"createdAt"`
	UpdatedAt         time.Time               `json:"updatedAt"`
	LastRunner        string                  `json:"lastRunner"`
	CLIVersion        string                  `json:"cliVersion"`
	LastError         string                  `json:"lastError,omitempty"`
	Nodes             map[string]NodeProgress `json:"nodes"`
	Bootstrap         *BootstrapProgress      `json:"bootstrap,omitempty"`
	Deployment        *DeploymentProgress     `json:"deployment,omitempty"`
	Runtime           *RuntimeState           `json:"runtime,omitempty"`
	Upgrade           *UpgradeProgress        `json:"upgrade,omitempty"`
}

func NewRecord(p Plan) Record {
	now := time.Now().UTC()
	r := Record{APIVersion: p.APIVersion, Kind: "EC2ProvisionOperation", Plan: p, Phase: "pending", ApplicationHealth: "unknown", CreatedAt: now, UpdatedAt: now, Nodes: map[string]NodeProgress{}}
	for _, t := range p.Targets {
		r.Nodes[t.Name] = NodeProgress{Phase: "pending", EC2State: "unknown"}
	}
	return r
}

func (r Record) Validate(p Plan) error {
	if err := r.Plan.Validate(); err != nil {
		return err
	}
	if r.Revision < 0 || !reflect.DeepEqual(r.Plan, p) || r.APIVersion != p.APIVersion || r.Kind != "EC2ProvisionOperation" || r.ApplicationHealth != "unknown" || len(r.Nodes) != len(p.Targets) || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		return errors.New("operation journal identity, schema, or target set mismatch")
	}
	switch r.Phase {
	case "pending", "provisioning", "interrupted", "compute-ready":
	default:
		return errors.New("invalid operation phase")
	}
	for _, t := range p.Targets {
		node, ok := r.Nodes[t.Name]
		if !ok {
			return fmt.Errorf("journal lost target %s", t.Name)
		}
		switch node.Phase {
		case "pending":
			if node.InstanceID != "" || !node.AttemptedAt.IsZero() {
				return errors.New("invalid pending target")
			}
		case "launching":
			if node.AttemptedAt.IsZero() || node.InstanceID != "" {
				return errors.New("invalid launching target")
			}
		case "present":
			if node.AttemptedAt.IsZero() || node.InstanceID == "" || node.ObservedAt.IsZero() {
				return errors.New("invalid present target")
			}
		default:
			return errors.New("invalid target phase")
		}
	}
	if err := r.validateBootstrap(p); err != nil {
		return err
	}
	if err := r.validateDeployment(p); err != nil {
		return err
	}
	return r.validateUpgrade(p)
}

// Store serializes CLI and Actions through a non-expiring owner claim. The same
// network key is permanently bound to this immutable plan in the first executor.
// Failed or cancelled work retains its record; it is never rolled back implicitly.
// Acquire errors are ambiguous: a lost response may have left a claim. Never
// blindly release after a failed Acquire. Inspect the caller's printed runner ID.
// Save requires the next revision, atomically conditional on its predecessor: a
// delayed retry from this same owner must not regress newer target checkpoints.
type Store interface {
	Acquire(context.Context, Plan, string) (Record, error)
	Save(context.Context, Record, string) error
	Release(context.Context, Plan, string) error
}

// Execute verifies the footprint, records launch intent BEFORE each AWS request,
// reconciles every target, and waits for EC2 running. EC2 running is not node or
// application health. No SSH, bootstrap, registration, reset, or terminate call.
func Execute(ctx context.Context, p Plan, identity inventory.STS, cloud EC2, store Store, owner, version string, pollInterval time.Duration, progress func(string)) (result Record, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	if owner == "" || pollInterval <= 0 {
		return result, errors.New("runner ID and positive poll interval required")
	}
	verified, verifyErr := Prepare(ctx, p.Network, identity, cloud)
	if verifyErr != nil {
		return result, verifyErr
	}
	if verified.ID != p.ID {
		return result, errors.New("live footprint differs from approved plan; regenerate and review")
	}
	r, err := store.Acquire(ctx, p, owner)
	if err != nil {
		return result, fmt.Errorf("claim operation: %w", err)
	}
	// Release only after this runner has stopped issuing AWS requests. A hard kill
	// intentionally leaves the non-expiring claim for explicit operator recovery.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err != nil && r.Validate(p) == nil {
			r.Phase = "interrupted"
			r.LastError = err.Error()
			if len(r.LastError) > 4096 {
				r.LastError = r.LastError[:4096]
			}
			r.UpdatedAt = time.Now().UTC()
			r.Revision++
			if saveErr := store.Save(cleanup, r, owner); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("save interruption: %w", saveErr))
			}
		}
		if releaseErr := store.Release(cleanup, p, owner); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release runner claim; inspect operation before retrying: %w", releaseErr))
		}
		result = r
	}()
	if err = r.Validate(p); err != nil {
		return
	}
	r.LastRunner = owner
	r.CLIVersion = version
	r.Phase = "provisioning"
	r.LastError = ""
	save := func() error { r.UpdatedAt = time.Now().UTC(); r.Revision++; return store.Save(ctx, r, owner) }
	if err = save(); err != nil {
		return
	}
	report := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	// Validate the entire live scope before the first mutation. A legacy or foreign
	// instance with the selected network tag prevents adoption, even on target 100.
	live, err := discover(ctx, p, cloud)
	if err != nil {
		return
	}
	if err = reconcile(p, &r, live, false); err != nil {
		return
	}
	if err = save(); err != nil {
		return
	}
	for _, t := range p.Targets {
		node := r.Nodes[t.Name]
		if node.Phase == "present" {
			continue
		}
		if err = ctx.Err(); err != nil {
			return
		}
		// Reassert ownership via a conditional journal write immediately before AWS.
		node.Phase = "launching"
		node.AttemptedAt = time.Now().UTC()
		r.Nodes[t.Name] = node
		if err = save(); err != nil {
			return
		}
		report("launching " + t.Name)
		var id string
		id, err = launch(ctx, p, t, cloud)
		if err != nil {
			err = fmt.Errorf("launch %s (outcome may be uncertain; resume reconciles, never blindly relaunches): %w", t.Name, err)
			return
		}
		node.InstanceID = id
		node.Phase = "present"
		node.EC2State = "pending"
		node.ObservedAt = time.Now().UTC()
		r.Nodes[t.Name] = node
		if err = save(); err != nil {
			return
		}
	}
	for {
		live, err = discover(ctx, p, cloud)
		if err != nil {
			return
		}
		if err = reconcile(p, &r, live, true); err != nil {
			return
		}
		ready := true
		for _, n := range r.Nodes {
			if n.EC2State != "running" {
				ready = false
			}
		}
		if ready {
			r.Phase = "compute-ready"
		}
		if err = save(); err != nil {
			return
		}
		if ready {
			report("all targets EC2 running; application health remains unknown")
			return
		}
		report("waiting for all targets to become EC2 running")
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
			return
		case <-timer.C:
		}
	}
}
