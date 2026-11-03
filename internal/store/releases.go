package store

import (
	"context"
	"encoding/json"
	"time"

	"foundry.dev/foundry/internal/ids"
	"github.com/jackc/pgx/v5"
)

// Release is a published, signed build (immutable).
type Release struct {
	ID        string          `json:"id"`
	OrgID     string          `json:"orgId"`
	ProjectID string          `json:"projectId"`
	BuildID   string          `json:"buildId"`
	Version   string          `json:"version"`
	Arch      string          `json:"arch"`
	Roothash  string          `json:"roothash"`
	PCR11     json.RawMessage `json:"pcr11"`
	Tier      string          `json:"tier"`
	RevokedAt *time.Time      `json:"revokedAt,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Publish transitions signed→published and creates the release atomically.
func (db *DB) Publish(ctx context.Context, b *Build, version string) (*Release, error) {
	rel := &Release{ID: ids.New("rel"), OrgID: b.OrgID, ProjectID: b.ProjectID, BuildID: b.ID, Version: version, Arch: b.Arch, Roothash: b.Roothash}
	tier := "private-unverified"
	if b.Tier != nil {
		tier = *b.Tier
	}
	rel.Tier = tier
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		if err := Transition(ctx, tx, b.ID, StateSigned, StatePublished, TransitionOpts{Actor: "publisher", Detail: map[string]string{"release": rel.ID}}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO releases (id, org_id, project_id, build_id, version, arch, usr_roothash, pcr11_predictions, tier)
			SELECT $1, $2, $3, $4, $5, $6, usr_roothash, COALESCE(pcr11, '{}'::jsonb), $7 FROM builds WHERE id=$4
			RETURNING pcr11_predictions, created_at`, rel.ID, b.OrgID, b.ProjectID, b.ID, version, b.Arch, tier).Scan(&rel.PCR11, &rel.CreatedAt)
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return rel, nil
}

const releaseCols = `id, org_id, project_id, build_id, version, arch, usr_roothash, pcr11_predictions, tier, revoked_at, created_at`

func scanRelease(r pgx.Row) (*Release, error) {
	var rel Release
	var rh []byte
	err := r.Scan(&rel.ID, &rel.OrgID, &rel.ProjectID, &rel.BuildID, &rel.Version, &rel.Arch, &rh, &rel.PCR11, &rel.Tier, &rel.RevokedAt, &rel.CreatedAt)
	if len(rh) > 0 {
		rel.Roothash = digestString(rh)[7:]
	}
	return &rel, mapErr(err)
}

// GetRelease by id within an org.
func (db *DB) GetRelease(ctx context.Context, orgID, id string) (*Release, error) {
	return scanRelease(db.Pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE org_id=$1 AND id=$2`, orgID, id))
}

// ReleaseForBuild returns the release of a build.
func (db *DB) ReleaseForBuild(ctx context.Context, buildID string) (*Release, error) {
	return scanRelease(db.Pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE build_id=$1`, buildID))
}

// ListReleases lists a project's releases, newest first.
func (db *DB) ListReleases(ctx context.Context, orgID, projectID string, limit int) ([]Release, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+releaseCols+` FROM releases WHERE org_id=$1 AND ($2='' OR project_id=$2) ORDER BY created_at DESC, id DESC LIMIT $3`,
		orgID, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RevokeRelease marks a release and its build revoked.
func (db *DB) RevokeRelease(ctx context.Context, orgID, id, actor, reason string) error {
	return db.Tx(ctx, func(tx pgx.Tx) error {
		var buildID string
		if err := tx.QueryRow(ctx, `UPDATE releases SET revoked_at=now() WHERE org_id=$1 AND id=$2 AND revoked_at IS NULL RETURNING build_id`, orgID, id).Scan(&buildID); err != nil {
			return mapErr(err)
		}
		tier := "revoked"
		return Transition(ctx, tx, buildID, StatePublished, StateRevoked, TransitionOpts{Actor: actor, Tier: &tier, Detail: map[string]string{"reason": reason}})
	})
}
