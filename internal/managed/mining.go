package managed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Recovery is independently armed on each miner before generation is stopped.
// Expiry restores mining; it never grants permission for another withdrawal.
type MiningReceipt struct {
	ServiceHash string `json:"serviceHash"`
	Active      bool   `json:"active"`
	OperationID string `json:"operationId"`
	Paused      bool   `json:"paused"`
	ExpiresAt   int64  `json:"expiresAt"`
}

func miningTargets(p Plan) []Target {
	var out []Target
	if p.Migration != nil {
		for _, t := range p.Snapshot.Fleet.Targets {
			if slices.Contains(p.Migration.Miners, t.Name) {
				out = append(out, t)
			}
		}
	}
	return out
}

func (r Runner) miningCall(ctx context.Context, p Plan, t Target, action string) (Observation, error) {
	q := request(p.Snapshot.Fleet, t, action)
	q.OperationID = p.ID
	q.Operation = p.Operation
	q.Migration = p.Migration
	before := p.Snapshot.Nodes[t.Name]
	q.Expected = &before
	return r.Remote.Call(ctx, q)
}

func (r Runner) resumeMining(p Plan) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var result error
	for _, t := range miningTargets(p) {
		o, e := r.miningCall(ctx, p, t, "mining_resume")
		if e == nil && (o.Mining == nil || o.Mining.Paused || !o.Mining.Active) {
			e = errors.New("mining resume unverified")
		}
		if e != nil {
			result = errors.Join(result, fmt.Errorf("%s mining resume: %w", t.Name, e))
		}
	}
	return result
}

// Each target is fully brought back before mining resumes. Core progress is
// required by verify afterwards, never while our miners are intentionally held.
func (r Runner) applyWithMining(ctx context.Context, p Plan, t Target, q Request, fresh bool) (err error) {
	miners := miningTargets(p)
	if len(miners) == 0 || !fresh {
		_, err = r.Remote.Call(ctx, q)
		if err == nil && p.Operation == "upgrade" {
			err = r.awaitReady(ctx, p.Snapshot.Fleet, t)
		}
		return
	}
	admission := q
	admission.Action = "admit"
	if _, err = r.Remote.Call(ctx, admission); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.resumeMining(p)) }()
	var tip int64
	var tipHash string
	for _, m := range miners {
		o, e := r.miningCall(ctx, p, m, "mining_pause")
		if e != nil {
			return e
		}
		if o.Mining == nil || !o.Mining.Paused || o.Mining.OperationID != p.ID || o.Mining.ExpiresAt < time.Now().Add(12*time.Minute).Unix() {
			return errors.New("mining pause lease not verified")
		}
	}
	// All miners must still be held and agree after the last one has stopped.
	for _, m := range miners {
		o, e := r.miningCall(ctx, p, m, "mining_status")
		if e != nil {
			return e
		}
		if o.Mining == nil || !o.Mining.Paused || o.Mining.ExpiresAt < time.Now().Add(11*time.Minute).Unix() {
			return errors.New("mining pause expired before withdrawal")
		}
		if tip != 0 && (tip != o.Chain.CoreHeight || tipHash != o.Chain.CoreHash) {
			return errors.New("paused miners disagree on Core tip")
		}
		tip = o.Chain.CoreHeight
		tipHash = o.Chain.CoreHash
	}
	if tip < 1 || !digestRE.MatchString(tipHash) {
		return errors.New("paused Core checkpoint missing")
	}
	h, e := r.withdrawalHealth(ctx, p)
	if e != nil {
		return e
	}
	if !healthSelected(h, p) {
		return errors.New("fleet unhealthy after pausing miners")
	}
	if e = canWithdrawSelected(h.Snapshot, t, p.Images[t.Name]); e != nil {
		return e
	}
	q.CoreCheckpointHeight = tip
	q.CoreCheckpointHash = tipHash
	// This bounds controller work below the independent fifteen-minute recovery.
	// A failed remote apply remains resumable; never start another target on error.
	run, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	r.report("owned mining paused; applying " + t.Name)
	if _, err = r.Remote.Call(run, q); err != nil {
		return err
	}
	if err = r.awaitReady(run, p.Snapshot.Fleet, t); err != nil {
		return err
	}
	check := request(p.Snapshot.Fleet, t, "observe")
	check.CoreCheckpointHeight = tip
	o, e := r.Remote.Call(run, check)
	if e != nil {
		return e
	}
	if o.Chain.CoreCheckpointHash != tipHash || o.Chain.CoreHeight < tip {
		return errors.New("restarted node lost paused Core checkpoint")
	}
	for _, m := range miners {
		o, e := r.miningCall(run, p, m, "mining_status")
		if e != nil {
			return e
		}
		if o.Mining == nil || !o.Mining.Paused || o.Mining.ExpiresAt <= time.Now().Unix() {
			return errors.New("mining watchdog fired during restart; stop rollout")
		}
	}
	r.report("target ready; resuming owned mining before progression checks")
	return nil
}
