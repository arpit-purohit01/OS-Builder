package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"foundry.dev/foundry/internal/ids"
	"github.com/jackc/pgx/v5"
)

type Build struct {
	ID               string          `json:"id"`
	OrgID            string          `json:"orgId"`
	ProjectID        string          `json:"projectId"`
	SpecVersionID    string          `json:"specVersionId"`
	Arch             string          `json:"arch"`
	State            string          `json:"state"`
	Tier             *string         `json:"tier,omitempty"`
	RiskScore        *int            `json:"riskScore,omitempty"`
	HostID           *string         `json:"hostId,omitempty"`
	LeaseExpiresAt   *time.Time      `json:"leaseExpiresAt,omitempty"`
	Attempt          int             `json:"attempt"`
	Failure          json.RawMessage `json:"failure,omitempty"`
	Roothash         string          `json:"roothash,omitempty"`
	Reproduced       *bool           `json:"reproduced,omitempty"`
	CatalogVersion   string          `json:"catalogVersion"`
	TreeDigest       string          `json:"treeDigest,omitempty"`
	Platform         json.RawMessage `json:"platform,omitempty"`
	Priority         int             `json:"priority"`
	CancelRequested  bool            `json:"cancelRequested"`
	PredictedMinutes float64         `json:"predictedMinutes"`
	TestReport       json.RawMessage `json:"testReport,omitempty"`
	CreatedBy        string          `json:"createdBy"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	StartedAt        *time.Time      `json:"startedAt,omitempty"`
	FinishedAt       *time.Time      `json:"finishedAt,omitempty"`
}

const buildCols = `id, org_id, project_id, spec_version_id, arch, state, tier, risk_score, host_id, lease_expires_at, attempt, failure,
	usr_roothash, reproduced, COALESCE(catalog_version,''), tree_digest, platform, priority, cancel_requested, predicted_minutes, test_report,
	created_by, created_at, updated_at, started_at, finished_at`

func scanBuild(r pgx.Row) (*Build, error) {
	b := &Build{}
	var rh, td []byte
	var risk *int16
	var failure, platform, report []byte
	err := r.Scan(&b.ID, &b.OrgID, &b.ProjectID, &b.SpecVersionID, &b.Arch, &b.State, &b.Tier, &risk, &b.HostID, &b.LeaseExpiresAt,
		&b.Attempt, &failure, &rh, &b.Reproduced, &b.CatalogVersion, &td, &platform, &b.Priority, &b.CancelRequested, &b.PredictedMinutes, &report,
		&b.CreatedBy, &b.CreatedAt, &b.UpdatedAt, &b.StartedAt, &b.FinishedAt)
	if risk != nil {
		v := int(*risk)
		b.RiskScore = &v
	}
	if len(rh) > 0 {
		b.Roothash = digestString(rh)[7:]
	}
	b.TreeDigest = digestString(td)
	if len(failure) > 0 {
		b.Failure = failure
	}
	if len(platform) > 0 {
		b.Platform = platform
	}
	if len(report) > 0 {
		b.TestReport = report
	}
	return b, mapErr(err)
}

// CreateBuild inserts a queued build and its first event in one transaction,
// and enqueues the scheduler job.
func (db *DB) CreateBuild(ctx context.Context, b *Build) (*Build, error) {
	b.ID = ids.New("bld")
	b.State = StateQueued
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO builds (id, org_id, project_id, spec_version_id, arch, state, catalog_version, tree_digest, platform,
			priority, predicted_minutes, created_by) VALUES ($1,$2,$3,$4,$5,'queued',$6,$7,$8,$9,$10,$11)`,
			b.ID, b.OrgID, b.ProjectID, b.SpecVersionID, b.Arch, b.CatalogVersion, digestBytes(b.TreeDigest), nullRaw(b.Platform),
			b.Priority, b.PredictedMinutes, b.CreatedBy)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO build_events (build_id, org_id, seq, from_state, to_state, actor) VALUES ($1,$2,1,NULL,'queued',$3)`,
			b.ID, b.OrgID, b.CreatedBy)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return db.GetBuildAny(ctx, b.ID)
}

func (db *DB) GetBuild(ctx context.Context, orgID, id string) (*Build, error) {
	return scanBuild(db.Pool.QueryRow(ctx, `SELECT `+buildCols+` FROM builds WHERE org_id=$1 AND id=$2`, orgID, id))
}

// GetBuildAny is for internal services (scheduler, workers) not acting for a tenant.
func (db *DB) GetBuildAny(ctx context.Context, id string) (*Build, error) {
	return scanBuild(db.Pool.QueryRow(ctx, `SELECT `+buildCols+` FROM builds WHERE id=$1`, id))
}

type BuildFilter struct {
	ProjectID string
	State     string
}

func (db *DB) ListBuilds(ctx context.Context, orgID string, f BuildFilter, cur *Cursor, limit int) ([]Build, *Cursor, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+buildCols+` FROM builds WHERE org_id=$1
		AND ($2 = '' OR project_id=$2) AND ($3 = '' OR state=$3)
		AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5))
		ORDER BY created_at DESC, id DESC LIMIT $6`, orgID, f.ProjectID, f.State, cur.time(), cur.id(), limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, *b)
	}
	return page(out, limit, func(b Build) Cursor { return Cursor{T: b.CreatedAt, ID: b.ID} })
}

// QueuedBuilds returns queued builds oldest first for the scheduler.
func (db *DB) QueuedBuilds(ctx context.Context, limit int) ([]Build, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+buildCols+` FROM builds WHERE state='queued' ORDER BY priority DESC, created_at, id LIMIT $1`, limit)
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

// RunningCount returns builds in flight per org and per host.
func (db *DB) RunningCount(ctx context.Context) (perOrg, perHost map[string]int, err error) {
	rows, err := db.Pool.Query(ctx, `SELECT org_id, COALESCE(host_id,''), count(*) FROM builds WHERE state IN ('leased','building') GROUP BY 1,2`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	perOrg, perHost = map[string]int{}, map[string]int{}
	for rows.Next() {
		var o, h string
		var n int
		if err := rows.Scan(&o, &h, &n); err != nil {
			return nil, nil, err
		}
		perOrg[o] += n
		if h != "" {
			perHost[h] += n
		}
	}
	return perOrg, perHost, rows.Err()
}

// Lease assigns a queued build to a host with a lease.
func (db *DB) Lease(ctx context.Context, buildID, hostID string, ttl time.Duration) error {
	return db.Tx(ctx, func(tx pgx.Tx) error {
		if err := Transition(ctx, tx, buildID, StateQueued, StateLeased, TransitionOpts{Actor: "scheduler", HostID: &hostID, Detail: map[string]string{"host": hostID}}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE builds SET lease_expires_at=now()+$2::interval WHERE id=$1`, buildID, ttl.String())
		return err
	})
}

// RenewLease extends the lease if the host still owns the build.
func (db *DB) RenewLease(ctx context.Context, buildID, hostID string, ttl time.Duration) (cancel bool, err error) {
	err = db.Pool.QueryRow(ctx, `UPDATE builds SET lease_expires_at=now()+$3::interval WHERE id=$1 AND host_id=$2 AND state IN ('leased','building')
		RETURNING cancel_requested`, buildID, hostID, ttl.String()).Scan(&cancel)
	return cancel, mapErr(err)
}

// ExpiredLeases returns builds whose lease ran out (host died).
func (db *DB) ExpiredLeases(ctx context.Context) ([]Build, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+buildCols+` FROM builds WHERE state IN ('leased','building') AND lease_expires_at < now()`)
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

func (db *DB) RequestCancel(ctx context.Context, orgID, id string) (*Build, error) {
	b, err := db.GetBuild(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if b.State == StateQueued {
		if err := db.Transition(ctx, id, StateQueued, StateCancelled, TransitionOpts{Actor: "user"}); err != nil {
			return nil, err
		}
	} else if !Terminal(b.State) {
		if _, err := db.Pool.Exec(ctx, `UPDATE builds SET cancel_requested=true WHERE id=$1`, id); err != nil {
			return nil, err
		}
	}
	return db.GetBuild(ctx, orgID, id)
}

// SetBuildResult stores roothash/manifest after a successful build.
func (db *DB) SetBuildResult(ctx context.Context, id, roothash, manifestDigest string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE builds SET usr_roothash=$2, manifest_digest=$3 WHERE id=$1`, id, digestBytes(roothash), digestBytes(manifestDigest))
	return err
}

func (db *DB) SetTestReport(ctx context.Context, id string, report any) error {
	b, _ := json.Marshal(report)
	_, err := db.Pool.Exec(ctx, `UPDATE builds SET test_report=$2 WHERE id=$1`, id, b)
	return err
}

// CompareReproduced compares this build's usr root hash with the most recent
// other build of the same organization and compiled tree (same spec,
// catalog and platform). It records the verdict on both builds and returns
// the other build's id; ok is nil when there is nothing to compare with.
func (db *DB) CompareReproduced(ctx context.Context, id string) (other string, ok *bool, err error) {
	var same bool
	err = db.Pool.QueryRow(ctx, `
		SELECT o.id, o.usr_roothash = m.usr_roothash
		FROM builds m JOIN builds o ON o.org_id = m.org_id AND o.tree_digest = m.tree_digest AND o.id <> m.id
		WHERE m.id = $1 AND m.usr_roothash IS NOT NULL AND o.usr_roothash IS NOT NULL
		ORDER BY o.created_at DESC LIMIT 1`, id).Scan(&other, &same)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	for _, b := range []string{id, other} {
		if err := db.SetReproduced(ctx, b, same); err != nil {
			return "", nil, err
		}
	}
	return other, &same, nil
}

func (db *DB) SetReproduced(ctx context.Context, id string, ok bool) error {
	_, err := db.Pool.Exec(ctx, `UPDATE builds SET reproduced=$2 WHERE id=$1`, id, ok)
	return err
}

// ------------------------------------------------------------------ events

type BuildEvent struct {
	Seq    int             `json:"seq"`
	TS     time.Time       `json:"ts"`
	From   *string         `json:"from,omitempty"`
	To     string          `json:"to"`
	Actor  string          `json:"actor"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

func (db *DB) BuildEvents(ctx context.Context, orgID, id string) ([]BuildEvent, error) {
	rows, err := db.Pool.Query(ctx, `SELECT seq, ts, from_state, to_state, actor, detail FROM build_events WHERE org_id=$1 AND build_id=$2 ORDER BY seq`, orgID, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (BuildEvent, error) {
		var e BuildEvent
		var d []byte
		err := r.Scan(&e.Seq, &e.TS, &e.From, &e.To, &e.Actor, &d)
		if len(d) > 0 {
			e.Detail = d
		}
		return e, err
	})
}

// -------------------------------------------------------------------- logs

type LogLine struct {
	Seq    int       `json:"seq"`
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
}

// AppendLogs appends lines with consecutive sequence numbers (single writer per build).
func (db *DB) AppendLogs(ctx context.Context, buildID, stream string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	return db.Tx(ctx, func(tx pgx.Tx) error {
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0)+1 FROM build_logs WHERE build_id=$1`, buildID).Scan(&next); err != nil {
			return err
		}
		rows := make([][]any, len(lines))
		now := time.Now()
		for i, l := range lines {
			if len(l) > 4096 {
				l = l[:4096]
			}
			rows[i] = []any{buildID, next + i, now, stream, l}
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"build_logs"}, []string{"build_id", "seq", "ts", "stream", "line"}, pgx.CopyFromRows(rows))
		return err
	})
}

func (db *DB) Logs(ctx context.Context, orgID, buildID string, afterSeq, limit int) ([]LogLine, error) {
	rows, err := db.Pool.Query(ctx, `SELECT l.seq, l.ts, l.stream, l.line FROM build_logs l JOIN builds b ON b.id=l.build_id
		WHERE b.org_id=$1 AND l.build_id=$2 AND l.seq > $3 ORDER BY l.seq LIMIT $4`, orgID, buildID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LogLine])
}

// --------------------------------------------------------------- artifacts

type Artifact struct {
	BuildID string    `json:"buildId"`
	Kind    string    `json:"kind"`
	Digest  string    `json:"digest"`
	Size    int64     `json:"size"`
	Signed  bool      `json:"signed"`
	Created time.Time `json:"createdAt"`
}

func (db *DB) PutArtifact(ctx context.Context, orgID string, a Artifact) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO artifacts (build_id, org_id, kind, digest, size_bytes, signed) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (build_id, kind) DO UPDATE SET digest=EXCLUDED.digest, size_bytes=EXCLUDED.size_bytes, signed=EXCLUDED.signed`,
		a.BuildID, orgID, a.Kind, digestBytes(a.Digest), a.Size, a.Signed)
	return mapErr(err)
}

func (db *DB) Artifacts(ctx context.Context, orgID, buildID string) ([]Artifact, error) {
	rows, err := db.Pool.Query(ctx, `SELECT build_id, kind, digest, size_bytes, signed, created_at FROM artifacts WHERE org_id=$1 AND build_id=$2 ORDER BY kind`, orgID, buildID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Artifact, error) {
		var a Artifact
		var d []byte
		err := r.Scan(&a.BuildID, &a.Kind, &d, &a.Size, &a.Signed, &a.Created)
		a.Digest = digestString(d)
		return a, err
	})
}

// ------------------------------------------------------------- stage runs

type StageRun struct {
	Stage      string     `json:"stage"`
	CacheKey   string     `json:"cacheKey"`
	CacheHit   string     `json:"cacheHit"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	ExitCode   *int       `json:"exitCode,omitempty"`
}

func (db *DB) PutStageRun(ctx context.Context, buildID string, s StageRun) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO stage_runs (build_id, stage, cache_key, cache_hit, started_at, finished_at, exit_code) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (build_id, stage) DO UPDATE SET cache_hit=EXCLUDED.cache_hit, finished_at=EXCLUDED.finished_at, exit_code=EXCLUDED.exit_code`,
		buildID, s.Stage, digestBytes(s.CacheKey), s.CacheHit, s.StartedAt, s.FinishedAt, s.ExitCode)
	return err
}

func (db *DB) StageRuns(ctx context.Context, buildID string) ([]StageRun, error) {
	rows, err := db.Pool.Query(ctx, `SELECT stage, cache_key, cache_hit, started_at, finished_at, exit_code FROM stage_runs WHERE build_id=$1 ORDER BY started_at`, buildID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StageRun, error) {
		var s StageRun
		var k []byte
		err := r.Scan(&s.Stage, &k, &s.CacheHit, &s.StartedAt, &s.FinishedAt, &s.ExitCode)
		s.CacheKey = digestString(k)
		return s, err
	})
}

// StageP50 returns the median duration (minutes) of recent runs per stage (scheduler cost model).
func (db *DB) StageP50(ctx context.Context) (map[string]float64, error) {
	rows, err := db.Pool.Query(ctx, `SELECT stage, percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM finished_at-started_at)/60)
		FROM (SELECT * FROM stage_runs WHERE finished_at IS NOT NULL ORDER BY started_at DESC LIMIT 5000) s GROUP BY stage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var s string
		var m float64
		if err := rows.Scan(&s, &m); err != nil {
			return nil, err
		}
		out[s] = m
	}
	return out, rows.Err()
}

// LogMatch returns the first build log line containing substr (worker side,
// e.g. digests the build announced before running customer code).
func (db *DB) LogMatch(ctx context.Context, buildID, substr string) (string, error) {
	var line string
	err := db.Pool.QueryRow(ctx, `SELECT line FROM build_logs WHERE build_id=$1 AND strpos(line, $2) > 0 ORDER BY seq LIMIT 1`, buildID, substr).Scan(&line)
	return line, mapErr(err)
}
