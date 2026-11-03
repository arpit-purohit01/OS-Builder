package store

import (
	"context"
	"time"
)

// PreviewSession authorizes a browser's access to one preview VM.
type PreviewSession struct {
	ID        string     `json:"id"`
	OrgID     string     `json:"orgId"`
	BuildID   string     `json:"buildId"`
	UserID    string     `json:"userId"`
	RemoteID  string     `json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

func (db *DB) CreatePreview(ctx context.Context, p *PreviewSession) error {
	return mapErr(db.Pool.QueryRow(ctx, `INSERT INTO preview_sessions (id, org_id, build_id, user_id, remote_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`, p.ID, p.OrgID, p.BuildID, p.UserID, p.RemoteID, p.ExpiresAt).Scan(&p.CreatedAt))
}

func (db *DB) GetPreview(ctx context.Context, orgID, id string) (*PreviewSession, error) {
	p := &PreviewSession{}
	err := db.Pool.QueryRow(ctx, `SELECT id, org_id, build_id, user_id, remote_id, expires_at, stopped_at, created_at
		FROM preview_sessions WHERE org_id=$1 AND id=$2`, orgID, id).Scan(&p.ID, &p.OrgID, &p.BuildID, &p.UserID, &p.RemoteID, &p.ExpiresAt, &p.StoppedAt, &p.CreatedAt)
	return p, mapErr(err)
}

func (db *DB) StopPreview(ctx context.Context, orgID, id string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE preview_sessions SET stopped_at=now() WHERE org_id=$1 AND id=$2 AND stopped_at IS NULL`, orgID, id)
	return err
}

// ActivePreviews counts an org's unexpired sessions.
func (db *DB) ActivePreviews(ctx context.Context, orgID string) (int, error) {
	var n int
	err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM preview_sessions WHERE org_id=$1 AND stopped_at IS NULL AND expires_at > now()`, orgID).Scan(&n)
	return n, err
}
