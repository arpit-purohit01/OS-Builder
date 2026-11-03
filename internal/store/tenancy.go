package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"foundry.dev/foundry/internal/ids"
	"github.com/jackc/pgx/v5"
)

type Org struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Slug          string    `json:"slug"`
	Plan          string    `json:"plan"`
	Region        string    `json:"region"`
	ExpertAllowed bool      `json:"expertAllowed"`
	CreatedAt     time.Time `json:"createdAt"`
}

type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Membership struct {
	OrgID string `json:"orgId"`
	Role  string `json:"role"`
	Org   Org    `json:"org"`
}

// Roles ordered by privilege for convenience checks.
var RoleRank = map[string]int{"viewer": 1, "reviewer": 2, "fleet_operator": 2, "builder": 3, "admin": 4, "owner": 5}

func (db *DB) CreateOrg(ctx context.Context, name, slug, plan string) (*Org, error) {
	o := &Org{ID: ids.New("org"), Name: name, Slug: slug, Plan: plan, Region: "eu"}
	err := db.Pool.QueryRow(ctx, `INSERT INTO orgs (id, name, slug, plan) VALUES ($1,$2,$3,$4) RETURNING created_at, region, expert_allowed`,
		o.ID, name, slug, plan).Scan(&o.CreatedAt, &o.Region, &o.ExpertAllowed)
	return o, mapErr(err)
}

func (db *DB) GetOrg(ctx context.Context, id string) (*Org, error) {
	o := &Org{}
	err := db.Pool.QueryRow(ctx, `SELECT id, name, slug, plan, region, expert_allowed, created_at FROM orgs WHERE id=$1`, id).
		Scan(&o.ID, &o.Name, &o.Slug, &o.Plan, &o.Region, &o.ExpertAllowed, &o.CreatedAt)
	return o, mapErr(err)
}

func (db *DB) SetOrgExpert(ctx context.Context, id string, allowed bool) error {
	_, err := db.Pool.Exec(ctx, `UPDATE orgs SET expert_allowed=$2 WHERE id=$1`, id, allowed)
	return err
}

// UpsertUser finds a user by email or creates one.
func (db *DB) UpsertUser(ctx context.Context, email, displayName string) (*User, error) {
	u := &User{}
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO users (id, email, display_name) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE SET display_name = CASE WHEN users.display_name = '' THEN EXCLUDED.display_name ELSE users.display_name END
		RETURNING id, email, display_name, created_at`, ids.New("usr"), email, displayName).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt)
	return u, mapErr(err)
}

func (db *DB) GetUser(ctx context.Context, id string) (*User, error) {
	u := &User{}
	err := db.Pool.QueryRow(ctx, `SELECT id, email, display_name, created_at FROM users WHERE id=$1`, id).Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt)
	return u, mapErr(err)
}

func (db *DB) LinkOIDC(ctx context.Context, userID, issuer, subject string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE users SET oidc_issuer=$2, oidc_subject=$3 WHERE id=$1`, userID, issuer, subject)
	return mapErr(err)
}

func (db *DB) AddMember(ctx context.Context, orgID, userID, role string) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1,$2,$3)
		ON CONFLICT (org_id, user_id) DO UPDATE SET role=EXCLUDED.role`, orgID, userID, role)
	return mapErr(err)
}

func (db *DB) Memberships(ctx context.Context, userID string) ([]Membership, error) {
	rows, err := db.Pool.Query(ctx, `SELECT m.org_id, m.role, o.id, o.name, o.slug, o.plan, o.region, o.expert_allowed, o.created_at
		FROM memberships m JOIN orgs o ON o.id=m.org_id WHERE m.user_id=$1 ORDER BY o.name`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Membership, error) {
		var m Membership
		err := r.Scan(&m.OrgID, &m.Role, &m.Org.ID, &m.Org.Name, &m.Org.Slug, &m.Org.Plan, &m.Org.Region, &m.Org.ExpertAllowed, &m.Org.CreatedAt)
		return m, err
	})
}

func (db *DB) Role(ctx context.Context, orgID, userID string) (string, error) {
	var role string
	err := db.Pool.QueryRow(ctx, `SELECT role FROM memberships WHERE org_id=$1 AND user_id=$2`, orgID, userID).Scan(&role)
	return role, mapErr(err)
}

// ----------------------------------------------------------------- sessions

func hashToken(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

type Session struct {
	ID        string
	UserID    string
	CSRF      string
	ExpiresAt time.Time
}

// CreateSession returns the session and the secret cookie value (only once).
func (db *DB) CreateSession(ctx context.Context, userID, userAgent string, ttl time.Duration) (*Session, string, error) {
	secret := ids.Secret(32)
	s := &Session{ID: ids.New("ses"), UserID: userID, CSRF: ids.Secret(20), ExpiresAt: time.Now().Add(ttl)}
	_, err := db.Pool.Exec(ctx, `INSERT INTO sessions (id, token_hash, user_id, csrf, expires_at, user_agent) VALUES ($1,$2,$3,$4,$5,$6)`,
		s.ID, hashToken(secret), userID, s.CSRF, s.ExpiresAt, userAgent)
	return s, secret, mapErr(err)
}

// SessionByToken validates a cookie value and slides last_seen.
func (db *DB) SessionByToken(ctx context.Context, secret string) (*Session, error) {
	s := &Session{}
	err := db.Pool.QueryRow(ctx, `UPDATE sessions SET last_seen=now() WHERE token_hash=$1 AND expires_at > now()
		RETURNING id, user_id, csrf, expires_at`, hashToken(secret)).Scan(&s.ID, &s.UserID, &s.CSRF, &s.ExpiresAt)
	return s, mapErr(err)
}

func (db *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, id)
	return err
}

// ------------------------------------------------------------------ tokens

type APIToken struct {
	ID        string     `json:"id"`
	OrgID     string     `json:"orgId"`
	UserID    string     `json:"userId"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt time.Time  `json:"expiresAt"`
	LastUsed  *time.Time `json:"lastUsed,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// CreateToken returns the token and its secret ("fdy_pat_<secret>"), shown once.
func (db *DB) CreateToken(ctx context.Context, orgID, userID, name string, scopes []string, ttl time.Duration) (*APIToken, string, error) {
	secret := "fdy_pat_" + ids.Secret(30)
	t := &APIToken{ID: ids.New("tok"), OrgID: orgID, UserID: userID, Name: name, Scopes: scopes, ExpiresAt: time.Now().Add(ttl)}
	err := db.Pool.QueryRow(ctx, `INSERT INTO api_tokens (id, org_id, user_id, name, token_hash, scopes, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`,
		t.ID, orgID, userID, name, hashToken(secret), scopes, t.ExpiresAt).Scan(&t.CreatedAt)
	return t, secret, mapErr(err)
}

func (db *DB) TokenBySecret(ctx context.Context, secret string) (*APIToken, error) {
	t := &APIToken{}
	err := db.Pool.QueryRow(ctx, `UPDATE api_tokens SET last_used=now() WHERE token_hash=$1 AND expires_at > now() AND revoked_at IS NULL
		RETURNING id, org_id, user_id, name, scopes, expires_at, created_at`, hashToken(secret)).
		Scan(&t.ID, &t.OrgID, &t.UserID, &t.Name, &t.Scopes, &t.ExpiresAt, &t.CreatedAt)
	return t, mapErr(err)
}

func (db *DB) RevokeToken(ctx context.Context, orgID, id string) error {
	ct, err := db.Pool.Exec(ctx, `UPDATE api_tokens SET revoked_at=now() WHERE org_id=$1 AND id=$2 AND revoked_at IS NULL`, orgID, id)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// -------------------------------------------------------------- login codes

func (db *DB) CreateLoginCode(ctx context.Context, email, code string, ttl time.Duration) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO login_codes (id, email, code_hash, expires_at) VALUES ($1,$2,$3,$4)`,
		ids.New("lc"), email, hashToken(email+"|"+code), time.Now().Add(ttl))
	return err
}

// ConsumeLoginCode validates the most recent unused code (max 5 attempts).
func (db *DB) ConsumeLoginCode(ctx context.Context, email, code string) (bool, error) {
	var id string
	var hash []byte
	var attempts int
	err := db.Pool.QueryRow(ctx, `SELECT id, code_hash, attempts FROM login_codes
		WHERE email=$1 AND used_at IS NULL AND expires_at > now() ORDER BY created_at DESC LIMIT 1`, email).Scan(&id, &hash, &attempts)
	if err != nil {
		return false, mapErr(err)
	}
	if attempts >= 5 {
		return false, nil
	}
	if string(hash) != string(hashToken(email+"|"+code)) {
		_, err = db.Pool.Exec(ctx, `UPDATE login_codes SET attempts=attempts+1 WHERE id=$1`, id)
		return false, err
	}
	ct, err := db.Pool.Exec(ctx, `UPDATE login_codes SET used_at=now() WHERE id=$1 AND used_at IS NULL`, id)
	return err == nil && ct.RowsAffected() == 1, err
}

// --------------------------------------------------------------- oidc state

func (db *DB) PutOIDCState(ctx context.Context, state, verifier, nonce, redirect string, ttl time.Duration) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO oidc_states (state, verifier, nonce, redirect, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		state, verifier, nonce, redirect, time.Now().Add(ttl))
	return err
}

func (db *DB) TakeOIDCState(ctx context.Context, state string) (verifier, nonce, redirect string, err error) {
	err = db.Pool.QueryRow(ctx, `DELETE FROM oidc_states WHERE state=$1 AND expires_at > now() RETURNING verifier, nonce, redirect`, state).
		Scan(&verifier, &nonce, &redirect)
	return verifier, nonce, redirect, mapErr(err)
}

// ------------------------------------------------------------- idempotency

type IdemRecord struct {
	RequestHash []byte
	Status      int
	Response    json.RawMessage
}

func (db *DB) GetIdempotency(ctx context.Context, orgID, key string) (*IdemRecord, error) {
	r := &IdemRecord{}
	err := db.Pool.QueryRow(ctx, `SELECT request_hash, status_code, response FROM idempotency_keys
		WHERE org_id=$1 AND key=$2 AND created_at > now() - interval '24 hours'`, orgID, key).Scan(&r.RequestHash, &r.Status, &r.Response)
	return r, mapErr(err)
}

func (db *DB) PutIdempotency(ctx context.Context, orgID, key string, reqHash []byte, status int, resp []byte) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO idempotency_keys (org_id, key, request_hash, status_code, response) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (org_id, key) DO NOTHING`, orgID, key, reqHash, status, resp)
	return err
}
