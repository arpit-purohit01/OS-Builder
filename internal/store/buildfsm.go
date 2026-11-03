package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Build states (docs/context/build-pipeline.md §7).
const (
	StateQueued           = "queued"
	StateLeased           = "leased"
	StateBuilding         = "building"
	StateBuilt            = "built"
	StateScanning         = "scanning"
	StateScanned          = "scanned"
	StateTesting          = "testing"
	StateTested           = "tested"
	StateAwaitingReview   = "awaiting_review"
	StateApproved         = "approved"
	StateChangesRequested = "changes_requested"
	StateRejected         = "rejected"
	StateFinalizing       = "finalizing"
	StateSigned           = "signed"
	StatePublished        = "published"
	StateRevoked          = "revoked"
	StateFailed           = "failed"
	StateCancelled        = "cancelled"
)

// transitions is the only definition of legal build state changes.
var transitions = map[string][]string{
	StateQueued:           {StateLeased, StateCancelled, StateFailed},
	StateLeased:           {StateBuilding, StateQueued, StateCancelled, StateFailed},
	StateBuilding:         {StateBuilt, StateQueued, StateFailed, StateCancelled},
	StateBuilt:            {StateScanning, StateFailed, StateCancelled},
	StateScanning:         {StateScanned, StateFailed, StateBuilt},
	StateScanned:          {StateTesting, StateFailed},
	StateTesting:          {StateTested, StateFailed, StateScanned},
	StateTested:           {StateApproved, StateAwaitingReview, StateRejected},
	StateAwaitingReview:   {StateApproved, StateRejected, StateChangesRequested},
	StateApproved:         {StateFinalizing},
	StateFinalizing:       {StateSigned, StateApproved, StateFailed},
	StateSigned:           {StatePublished, StateRevoked},
	StatePublished:        {StateRevoked},
	StateChangesRequested: {},
	StateRejected:         {},
	StateRevoked:          {},
	StateFailed:           {},
	StateCancelled:        {},
}

// Terminal reports whether no further transitions are possible.
func Terminal(state string) bool { return len(transitions[state]) == 0 }

// Allowed reports whether from→to is legal.
func Allowed(from, to string) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ErrIllegalTransition is returned for transitions not in the table.
var ErrIllegalTransition = fmt.Errorf("illegal build state transition")

// TransitionOpts carries optional column updates applied atomically with a transition.
type TransitionOpts struct {
	Actor   string
	Detail  any
	HostID  *string
	Failure any // JSON {stage, reason, retryable}
	Tier    *string
	Risk    *int
}

// Transition moves a build from → to inside tx, with an optimistic check on
// the current state, and appends a build_events row.
func Transition(ctx context.Context, tx pgx.Tx, buildID, from, to string, o TransitionOpts) error {
	if !Allowed(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
	}
	var failure []byte
	if o.Failure != nil {
		failure, _ = json.Marshal(o.Failure)
	}
	ct, err := tx.Exec(ctx, `
		UPDATE builds SET state=$3, updated_at=now(),
		  host_id = COALESCE($4, host_id),
		  failure = COALESCE($5::jsonb, failure),
		  tier = COALESCE($6, tier),
		  risk_score = COALESCE($7, risk_score),
		  started_at = CASE WHEN $3 = 'building' AND started_at IS NULL THEN now() ELSE started_at END,
		  finished_at = CASE WHEN $3 IN ('built','failed','cancelled') THEN now() ELSE finished_at END,
		  attempt = CASE WHEN $2 IN ('leased','building') AND $3 = 'queued' THEN attempt + 1 ELSE attempt END,
		  lease_expires_at = CASE WHEN $3 IN ('leased','building') THEN lease_expires_at ELSE NULL END
		WHERE id=$1 AND state=$2`, buildID, from, to, o.HostID, nullJSON(failure), o.Tier, o.Risk)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("%w: build %s not in state %s", ErrConflict, buildID, from)
	}
	detail, _ := json.Marshal(o.Detail)
	actor := o.Actor
	if actor == "" {
		actor = "system"
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO build_events (build_id, org_id, seq, from_state, to_state, actor, detail)
		SELECT $1, b.org_id, COALESCE((SELECT max(seq) FROM build_events WHERE build_id=$1), 0) + 1, $2, $3, $4, $5::jsonb
		FROM builds b WHERE b.id=$1`, buildID, from, to, actor, nullJSON(detail))
	return err
}

// TransitionDB runs Transition in its own transaction.
func (db *DB) Transition(ctx context.Context, buildID, from, to string, o TransitionOpts) error {
	return db.Tx(ctx, func(tx pgx.Tx) error { return Transition(ctx, tx, buildID, from, to, o) })
}

func nullJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}
