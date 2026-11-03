package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"foundry.dev/foundry/internal/jcs"
	"github.com/jackc/pgx/v5"
)

// AuditEntry is one append-only, hash-chained audit record.
type AuditEntry struct {
	ID     int64           `json:"id"`
	OrgID  string          `json:"orgId,omitempty"`
	TS     time.Time       `json:"ts"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
	Hash   []byte          `json:"hash"`
}

func auditHash(prev []byte, org, actor, action, target string, ts time.Time, detail []byte) []byte {
	if len(detail) > 0 { // jsonb re-serializes; hash the canonical form
		if c, err := jcs.Canonicalize(detail); err == nil {
			detail = c
		}
	}
	canon, _ := jcs.Marshal(map[string]any{"org": org, "actor": actor, "action": action, "target": target,
		"ts": ts.UTC().Format(time.RFC3339Nano), "detail": string(detail)})
	h := sha256.New()
	h.Write(prev)
	h.Write(canon)
	return h.Sum(nil)
}

// Audit appends an entry: hᵢ = SHA256(hᵢ₋₁ ‖ JCS(entry)). Serialized by an
// advisory lock so the chain is linear.
func (db *DB) Audit(ctx context.Context, orgID, actor, action, target string, detail any) error {
	var d []byte
	if detail != nil {
		d, _ = json.Marshal(detail)
	}
	return db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('foundry-audit'))`); err != nil {
			return err
		}
		prev := make([]byte, 32)
		err := tx.QueryRow(ctx, `SELECT hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&prev)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		ts := time.Now().UTC().Truncate(time.Microsecond)
		h := auditHash(prev, orgID, actor, action, target, ts, d)
		_, err = tx.Exec(ctx, `INSERT INTO audit_log (org_id, ts, actor, action, target, detail, prev_hash, hash) VALUES (NULLIF($1,''),$2,$3,$4,$5,$6,$7,$8)`,
			orgID, ts, actor, action, target, nullJSON(d), prev, h)
		return err
	})
}

// VerifyAuditChain recomputes the chain; returns the id of the first broken entry (0 = intact).
func (db *DB) VerifyAuditChain(ctx context.Context) (int64, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, COALESCE(org_id,''), ts, actor, action, COALESCE(target,''), detail, prev_hash, hash FROM audit_log ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	prev := make([]byte, 32)
	for rows.Next() {
		var id int64
		var org, actor, action, target string
		var ts time.Time
		var detail, p, h []byte
		if err := rows.Scan(&id, &org, &ts, &actor, &action, &target, &detail, &p, &h); err != nil {
			return 0, err
		}
		if string(p) != string(prev) || string(auditHash(prev, org, actor, action, target, ts, detail)) != string(h) {
			return id, nil
		}
		prev = h
	}
	return 0, rows.Err()
}

// AuditLog lists entries for an org (newest first).
func (db *DB) AuditLog(ctx context.Context, orgID string, limit int) ([]AuditEntry, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, COALESCE(org_id,''), ts, actor, action, COALESCE(target,''), detail, hash FROM audit_log WHERE org_id=$1 ORDER BY id DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		var d []byte
		err := r.Scan(&e.ID, &e.OrgID, &e.TS, &e.Actor, &e.Action, &e.Target, &d, &e.Hash)
		if len(d) > 0 {
			e.Detail = d
		}
		return e, err
	})
}
