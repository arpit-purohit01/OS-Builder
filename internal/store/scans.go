package store

import (
	"context"
	"encoding/json"
	"time"

	"foundry.dev/foundry/internal/ids"
	"github.com/jackc/pgx/v5"
)

// Scan is one scanner run over a build.
type Scan struct {
	ID          string     `json:"id"`
	BuildID     string     `json:"buildId"`
	Scanner     string     `json:"scanner"`
	ToolVersion string     `json:"toolVersion"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Report      string     `json:"reportDigest,omitempty"`
}

// FindingRow is a stored finding.
type FindingRow struct {
	ID             string          `json:"id"`
	ScanID         string          `json:"scanId"`
	BuildID        string          `json:"buildId"`
	Rule           string          `json:"rule"`
	Severity       string          `json:"severity"`
	Path           string          `json:"path,omitempty"`
	Weight         int             `json:"weight"`
	Detail         json.RawMessage `json:"detail"`
	AcceptedBy     *string         `json:"acceptedBy,omitempty"`
	AcceptedReason *string         `json:"acceptedReason,omitempty"`
}

// RecordScan stores a finished scan with its findings in one transaction.
func (db *DB) RecordScan(ctx context.Context, s Scan, findings []FindingRow) (*Scan, error) {
	s.ID = ids.New("scn")
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO scans (id, build_id, scanner, tool_version, status, finished_at, report_digest)
			VALUES ($1,$2,$3,$4,$5,now(),$6) RETURNING started_at, finished_at`, s.ID, s.BuildID, s.Scanner, s.ToolVersion, s.Status,
			digestBytes(s.Report)).Scan(&s.StartedAt, &s.FinishedAt); err != nil {
			return err
		}
		for _, f := range findings {
			if len(f.Detail) == 0 {
				f.Detail = json.RawMessage("{}")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO findings (id, scan_id, build_id, rule_id, severity, path, weight, detail) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8)`,
				ids.New("fnd"), s.ID, s.BuildID, f.Rule, f.Severity, f.Path, f.Weight, []byte(f.Detail)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &s, nil
}

// Scans lists a build's scans.
func (db *DB) Scans(ctx context.Context, buildID string) ([]Scan, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, build_id, scanner, tool_version, status, started_at, finished_at, report_digest FROM scans WHERE build_id=$1 ORDER BY started_at`, buildID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Scan, error) {
		var s Scan
		var d []byte
		err := r.Scan(&s.ID, &s.BuildID, &s.Scanner, &s.ToolVersion, &s.Status, &s.StartedAt, &s.FinishedAt, &d)
		s.Report = digestString(d)
		return s, err
	})
}

// Findings lists a build's findings, most severe first.
func (db *DB) Findings(ctx context.Context, buildID string) ([]FindingRow, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, scan_id, build_id, rule_id, severity, COALESCE(path,''), weight, detail, accepted_by, accepted_reason
		FROM findings WHERE build_id=$1 ORDER BY weight DESC, rule_id, id`, buildID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[FindingRow])
}

// AcceptFinding records a reviewer's acceptance of a finding.
func (db *DB) AcceptFinding(ctx context.Context, buildID, findingID, userID, reason string) error {
	ct, err := db.Pool.Exec(ctx, `UPDATE findings SET accepted_by=$3, accepted_reason=$4 WHERE build_id=$1 AND id=$2`, buildID, findingID, userID, reason)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Review is one reviewer decision.
type Review struct {
	ID        string    `json:"id"`
	BuildID   string    `json:"buildId"`
	Reviewer  string    `json:"reviewer"`
	Decision  string    `json:"decision"`
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// AddReview inserts a decision (one per reviewer per build).
func (db *DB) AddReview(ctx context.Context, tx pgx.Tx, r *Review) error {
	r.ID = ids.New("rev")
	return mapErr(tx.QueryRow(ctx, `INSERT INTO reviews (id, build_id, reviewer, decision, comment) VALUES ($1,$2,$3,$4,NULLIF($5,'')) RETURNING created_at`,
		r.ID, r.BuildID, r.Reviewer, r.Decision, r.Comment).Scan(&r.CreatedAt))
}

// Reviews lists a build's reviews.
func (db *DB) Reviews(ctx context.Context, buildID string) ([]Review, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, build_id, reviewer, decision, COALESCE(comment,''), created_at FROM reviews WHERE build_id=$1 ORDER BY created_at`, buildID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Review])
}

// ReviewQueue lists builds awaiting review across all orgs (platform
// reviewers) or one org, highest risk first.
func (db *DB) ReviewQueue(ctx context.Context, orgID string, limit int) ([]Build, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+buildCols+` FROM builds WHERE state='awaiting_review' AND ($1='' OR org_id=$1)
		ORDER BY risk_score DESC NULLS LAST, created_at LIMIT $2`, orgID, limit)
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
