package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Host is a worker machine registered by its agent.
type Host struct {
	ID       string          `json:"id"`
	Pool     string          `json:"pool"`
	Arch     string          `json:"arch"`
	Slots    int             `json:"slots"`
	Weight   float64         `json:"weight"`
	Labels   json.RawMessage `json:"labels,omitempty"`
	LastSeen time.Time       `json:"lastSeen"`
	Draining bool            `json:"draining"`
	Version  string          `json:"version"`
}

// UpsertHost registers or refreshes a host (heartbeat).
func (db *DB) UpsertHost(ctx context.Context, h Host) error {
	if h.Weight == 0 {
		h.Weight = 1
	}
	if h.Labels == nil {
		h.Labels = json.RawMessage("{}")
	}
	_, err := db.Pool.Exec(ctx, `INSERT INTO hosts (id, pool, arch, slots, weight, labels, last_seen, draining, version)
		VALUES ($1,$2,$3,$4,$5,$6,now(),$7,$8)
		ON CONFLICT (id) DO UPDATE SET pool=EXCLUDED.pool, arch=EXCLUDED.arch, slots=EXCLUDED.slots, weight=EXCLUDED.weight,
		  labels=EXCLUDED.labels, last_seen=now(), draining=EXCLUDED.draining, version=EXCLUDED.version`,
		h.ID, h.Pool, h.Arch, h.Slots, h.Weight, []byte(h.Labels), h.Draining, h.Version)
	return err
}

// LiveHosts returns non-draining hosts of a pool seen within maxAge.
func (db *DB) LiveHosts(ctx context.Context, pool string, maxAge time.Duration) ([]Host, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, pool, arch, slots, weight, labels, last_seen, draining, version FROM hosts
		WHERE pool=$1 AND NOT draining AND last_seen > now() - $2::interval ORDER BY id`, pool, maxAge.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Host, error) {
		var h Host
		var labels []byte
		err := r.Scan(&h.ID, &h.Pool, &h.Arch, &h.Slots, &h.Weight, &labels, &h.LastSeen, &h.Draining, &h.Version)
		h.Labels = labels
		return h, err
	})
}

// LeasedTo returns builds leased to a host that the host has not started yet.
func (db *DB) LeasedTo(ctx context.Context, hostID string) ([]Build, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+buildCols+` FROM builds WHERE host_id=$1 AND state='leased' ORDER BY priority DESC, created_at`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// BuildFormats returns the output formats requested for a build.
func (db *DB) BuildFormats(ctx context.Context, id string) ([]string, error) {
	var f []string
	err := db.Pool.QueryRow(ctx, `SELECT formats FROM builds WHERE id=$1`, id).Scan(&f)
	return f, mapErr(err)
}

// SetBuildFormats records requested output formats (at creation).
func (db *DB) SetBuildFormats(ctx context.Context, id string, formats []string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE builds SET formats=$2 WHERE id=$1`, id, formats)
	return err
}

// SetPCR11 stores PCR 11 predictions computed at finalize.
func (db *DB) SetPCR11(ctx context.Context, id string, v any) error {
	b, _ := json.Marshal(v)
	_, err := db.Pool.Exec(ctx, `UPDATE builds SET pcr11=$2 WHERE id=$1`, id, b)
	return err
}

// PCR11 returns stored predictions.
func (db *DB) PCR11(ctx context.Context, id string) (json.RawMessage, error) {
	var b []byte
	err := db.Pool.QueryRow(ctx, `SELECT pcr11 FROM builds WHERE id=$1`, id).Scan(&b)
	return b, mapErr(err)
}

// Notify sends a lightweight event on a Postgres channel (build progress wake-ups).
func (db *DB) Notify(ctx context.Context, channel, payload string) error {
	_, err := db.Pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, payload)
	return err
}

// SetEnvelope stores the signed job envelope of a build.
func (db *DB) SetEnvelope(ctx context.Context, id string, env any) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = db.Pool.Exec(ctx, `UPDATE builds SET envelope=$2 WHERE id=$1`, id, b)
	return err
}

// Envelope returns a build's signed job envelope (nil if none).
func (db *DB) Envelope(ctx context.Context, id string) (json.RawMessage, error) {
	var b []byte
	err := db.Pool.QueryRow(ctx, `SELECT envelope FROM builds WHERE id=$1`, id).Scan(&b)
	return b, mapErr(err)
}

// EgressEvent is one sandbox network request seen by the egress proxy.
type EgressEvent struct {
	BuildID string    `json:"buildId"`
	TS      time.Time `json:"ts"`
	Method  string    `json:"method"`
	Host    string    `json:"host"`
	Port    int       `json:"port"`
	Allowed bool      `json:"allowed"`
	Reason  string    `json:"reason,omitempty"`
	Bytes   int64     `json:"bytes"`
}

// RecordEgress stores an egress event (builds that no longer exist are ignored).
func (db *DB) RecordEgress(ctx context.Context, e EgressEvent) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO build_egress (build_id, ts, method, host, port, allowed, reason, bytes)
		SELECT $1,$2,$3,$4,$5,$6,NULLIF($7,''),$8 WHERE EXISTS (SELECT 1 FROM builds WHERE id=$1)`,
		e.BuildID, e.TS, e.Method, e.Host, e.Port, e.Allowed, e.Reason, e.Bytes)
	return err
}

// Egress lists a build's egress events (denied first).
func (db *DB) Egress(ctx context.Context, buildID string, deniedOnly bool) ([]EgressEvent, error) {
	rows, err := db.Pool.Query(ctx, `SELECT build_id, ts, method, host, port, allowed, COALESCE(reason,''), bytes FROM build_egress
		WHERE build_id=$1 AND (NOT $2 OR NOT allowed) ORDER BY allowed, ts LIMIT 1000`, buildID, deniedOnly)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[EgressEvent])
}
