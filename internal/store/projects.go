package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"foundry.dev/foundry/internal/ids"
	"github.com/jackc/pgx/v5"
)

type Project struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Name      string    `json:"name"`
	OSID      string    `json:"osId"`
	CreatedAt time.Time `json:"createdAt"`
}

type SpecVersion struct {
	ID             string          `json:"id"`
	OrgID          string          `json:"orgId"`
	ProjectID      string          `json:"projectId"`
	Digest         string          `json:"digest"`
	APIVersion     string          `json:"apiVersion"`
	Spec           json.RawMessage `json:"spec"`
	Source         string          `json:"source,omitempty"`
	ParentID       *string         `json:"parentId,omitempty"`
	Patch          json.RawMessage `json:"patchFromParent,omitempty"`
	CatalogVersion string          `json:"catalogVersion"`
	HasExpert      bool            `json:"hasExpert"`
	Warnings       json.RawMessage `json:"warnings"`
	CreatedBy      string          `json:"createdBy"`
	CreatedAt      time.Time       `json:"createdAt"`
}

func (db *DB) CreateProject(ctx context.Context, orgID, name, osID string) (*Project, error) {
	p := &Project{ID: ids.New("prj"), OrgID: orgID, Name: name, OSID: osID}
	err := db.Pool.QueryRow(ctx, `INSERT INTO projects (id, org_id, name, os_id) VALUES ($1,$2,$3,$4) RETURNING created_at`,
		p.ID, orgID, name, osID).Scan(&p.CreatedAt)
	return p, mapErr(err)
}

func (db *DB) GetProject(ctx context.Context, orgID, id string) (*Project, error) {
	p := &Project{}
	err := db.Pool.QueryRow(ctx, `SELECT id, org_id, name, os_id, created_at FROM projects WHERE org_id=$1 AND id=$2`, orgID, id).
		Scan(&p.ID, &p.OrgID, &p.Name, &p.OSID, &p.CreatedAt)
	return p, mapErr(err)
}

func (db *DB) ListProjects(ctx context.Context, orgID string) ([]Project, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, org_id, name, os_id, created_at FROM projects WHERE org_id=$1 ORDER BY created_at DESC, id DESC`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) {
		var p Project
		return p, r.Scan(&p.ID, &p.OrgID, &p.Name, &p.OSID, &p.CreatedAt)
	})
}

func digestBytes(d string) []byte {
	b, _ := hex.DecodeString(strings.TrimPrefix(d, "sha256:"))
	return b
}

func digestString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return "sha256:" + hex.EncodeToString(b)
}

// CreateSpecVersion inserts an immutable version; identical content returns the existing row (idempotent).
func (db *DB) CreateSpecVersion(ctx context.Context, v *SpecVersion) (*SpecVersion, bool, error) {
	v.ID = ids.New("spv")
	if v.Warnings == nil {
		v.Warnings = json.RawMessage("[]")
	}
	err := db.Pool.QueryRow(ctx, `INSERT INTO spec_versions (id, org_id, project_id, spec_digest, api_version, spec, source, parent_id,
		patch_from_parent, catalog_version, has_expert, warnings, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (project_id, spec_digest) DO NOTHING RETURNING created_at`,
		v.ID, v.OrgID, v.ProjectID, digestBytes(v.Digest), v.APIVersion, v.Spec, v.Source, v.ParentID, nullRaw(v.Patch),
		v.CatalogVersion, v.HasExpert, v.Warnings, v.CreatedBy).Scan(&v.CreatedAt)
	if err == pgx.ErrNoRows {
		existing, err := db.SpecVersionByDigest(ctx, v.OrgID, v.ProjectID, v.Digest)
		return existing, false, err
	}
	return v, true, mapErr(err)
}

const specCols = `id, org_id, project_id, spec_digest, api_version, spec, COALESCE(source,''), parent_id, patch_from_parent, catalog_version, has_expert, warnings, created_by, created_at`

func scanSpec(r pgx.Row) (*SpecVersion, error) {
	v := &SpecVersion{}
	var dig []byte
	var patch []byte
	err := r.Scan(&v.ID, &v.OrgID, &v.ProjectID, &dig, &v.APIVersion, &v.Spec, &v.Source, &v.ParentID, &patch, &v.CatalogVersion, &v.HasExpert, &v.Warnings, &v.CreatedBy, &v.CreatedAt)
	v.Digest = digestString(dig)
	if len(patch) > 0 {
		v.Patch = patch
	}
	return v, mapErr(err)
}

func (db *DB) GetSpecVersion(ctx context.Context, orgID, id string) (*SpecVersion, error) {
	return scanSpec(db.Pool.QueryRow(ctx, `SELECT `+specCols+` FROM spec_versions WHERE org_id=$1 AND id=$2`, orgID, id))
}

func (db *DB) SpecVersionByDigest(ctx context.Context, orgID, projectID, digest string) (*SpecVersion, error) {
	return scanSpec(db.Pool.QueryRow(ctx, `SELECT `+specCols+` FROM spec_versions WHERE org_id=$1 AND project_id=$2 AND spec_digest=$3`, orgID, projectID, digestBytes(digest)))
}

// ListSpecVersions uses keyset pagination on (created_at, id).
func (db *DB) ListSpecVersions(ctx context.Context, orgID, projectID string, cur *Cursor, limit int) ([]SpecVersion, *Cursor, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+specCols+` FROM spec_versions WHERE org_id=$1 AND project_id=$2
		AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4)) ORDER BY created_at DESC, id DESC LIMIT $5`,
		orgID, projectID, cur.time(), cur.id(), limit+1)
	if err != nil {
		return nil, nil, err
	}
	var out []SpecVersion
	for rows.Next() {
		v, err := scanSpec(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		out = append(out, *v)
	}
	rows.Close()
	return page(out, limit, func(v SpecVersion) Cursor { return Cursor{T: v.CreatedAt, ID: v.ID} })
}

// ------------------------------------------------------------------- assets

type Asset struct {
	Digest     string          `json:"digest"`
	OrgID      string          `json:"orgId"`
	Kind       string          `json:"kind"`
	MediaType  string          `json:"mediaType"`
	Size       int64           `json:"size"`
	Validation json.RawMessage `json:"validation"`
	Status     string          `json:"status"`
	CreatedAt  time.Time       `json:"createdAt"`
}

func (db *DB) PutAsset(ctx context.Context, a *Asset) error {
	if a.Validation == nil {
		a.Validation = json.RawMessage("{}")
	}
	_, err := db.Pool.Exec(ctx, `INSERT INTO assets (digest, org_id, kind, media_type, size_bytes, validation, status) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (org_id, digest) DO UPDATE SET validation=EXCLUDED.validation, status=EXCLUDED.status, kind=EXCLUDED.kind, media_type=EXCLUDED.media_type`,
		digestBytes(a.Digest), a.OrgID, a.Kind, a.MediaType, a.Size, a.Validation, a.Status)
	return mapErr(err)
}

func (db *DB) GetAsset(ctx context.Context, orgID, digest string) (*Asset, error) {
	a := &Asset{}
	var d []byte
	err := db.Pool.QueryRow(ctx, `SELECT digest, org_id, kind, media_type, size_bytes, validation, status, created_at FROM assets WHERE org_id=$1 AND digest=$2`,
		orgID, digestBytes(digest)).Scan(&d, &a.OrgID, &a.Kind, &a.MediaType, &a.Size, &a.Validation, &a.Status, &a.CreatedAt)
	a.Digest = digestString(d)
	return a, mapErr(err)
}

// ------------------------------------------------------------------ cursors

// Cursor is an opaque keyset position.
type Cursor struct {
	T  time.Time `json:"t"`
	ID string    `json:"i"`
}

func (c *Cursor) time() any {
	if c == nil {
		return nil
	}
	return c.T
}

func (c *Cursor) id() string {
	if c == nil {
		return ""
	}
	return c.ID
}

func page[T any](xs []T, limit int, key func(T) Cursor) ([]T, *Cursor, error) {
	if len(xs) > limit {
		xs = xs[:limit]
		c := key(xs[len(xs)-1])
		return xs, &c, nil
	}
	return xs, nil, nil
}

func nullRaw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// EnsureCatalogVersion registers a catalog version (idempotent) and optionally makes it the default.
func (db *DB) EnsureCatalogVersion(ctx context.Context, id string, cnfDigest []byte, makeDefault bool) error {
	return db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO catalog_versions (id, cnf_digest) VALUES ($1,$2) ON CONFLICT (id) DO NOTHING`, id, cnfDigest); err != nil {
			return err
		}
		if makeDefault {
			if _, err := tx.Exec(ctx, `UPDATE catalog_versions SET is_default=false WHERE is_default AND id<>$1`, id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE catalog_versions SET is_default=true WHERE id=$1`, id)
			return err
		}
		return nil
	})
}

// CatalogVersion represents a versioned catalog entry.
type CatalogVersion struct {
	ID           string    `json:"id"`
	CatalogID    string    `json:"catalog_id"` // git tree hash (same as ID for now)
	CNFDigest    []byte    `json:"cnf_digest"` // sha256 of compiled CNF
	Base         string    `json:"base"`       // base distro (e.g., ubuntu-24.04)
	Arch         string    `json:"arch"`       // architecture (e.g., x86_64)
	PublishedAt  time.Time `json:"published_at"`
	CreatedAt    time.Time `json:"created_at"` // alias for PublishedAt
	IsDefault    bool      `json:"is_default"`
	OptionsCount int       `json:"options_count"` // number of options in catalog
}

func scanCatalogVersion(r pgx.Row) (*CatalogVersion, error) {
	v := &CatalogVersion{}
	err := r.Scan(&v.ID, &v.CNFDigest, &v.PublishedAt, &v.IsDefault)
	if err != nil {
		return nil, mapErr(err)
	}
	v.CatalogID = v.ID // git tree hash
	v.CreatedAt = v.PublishedAt
	v.Base = "ubuntu-24.04" // default, not stored in DB
	v.Arch = "x86_64"       // default, not stored in DB
	v.OptionsCount = 0      // not stored in DB
	return v, nil
}

// ListCatalogVersions returns all catalog versions.
func (db *DB) ListCatalogVersions(ctx context.Context, orgID string) ([]CatalogVersion, error) {
	rows, err := db.Pool.Query(ctx, `SELECT id, cnf_digest, published_at, is_default FROM catalog_versions ORDER BY published_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogVersion
	for rows.Next() {
		v, err := scanCatalogVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// GetCatalogVersion returns a specific catalog version by ID.
func (db *DB) GetCatalogVersion(ctx context.Context, orgID, versionID string) (*CatalogVersion, error) {
	return scanCatalogVersion(db.Pool.QueryRow(ctx, `SELECT id, cnf_digest, published_at, is_default FROM catalog_versions WHERE id=$1`, versionID))
}

// CreateCatalogVersion creates a new catalog version.
func (db *DB) CreateCatalogVersion(ctx context.Context, orgID string, v *CatalogVersion) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO catalog_versions (id, cnf_digest) VALUES ($1,$2) ON CONFLICT (id) DO NOTHING`, v.ID, v.CNFDigest)
	return mapErr(err)
}

// CreateCatalogVersionWithDetails creates a new catalog version with full details (for API compatibility).
func (db *DB) CreateCatalogVersionWithDetails(ctx context.Context, orgID string, v *CatalogVersion) error {
	return db.CreateCatalogVersion(ctx, orgID, v)
}

// SetDefaultCatalogVersion marks a catalog version as the default.
func (db *DB) SetDefaultCatalogVersion(ctx context.Context, orgID, versionID string) error {
	return db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE catalog_versions SET is_default=false WHERE is_default`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE catalog_versions SET is_default=true WHERE id=$1`, versionID)
		return err
	})
}
