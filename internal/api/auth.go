package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"foundry.dev/foundry/internal/oidc"
	"foundry.dev/foundry/internal/store"
)

const (
	sessionCookie = "foundry_session"
	sessionTTL    = 12 * time.Hour
)

// principal is the authenticated caller.
type principal struct {
	UserID    string
	SessionID string
	CSRF      string
	Token     *store.APIToken // set for bearer tokens (org-bound)
	OrgID     string          // resolved org for org routes
	Role      string
}

func getPrincipal(ctx context.Context) *principal {
	p, _ := ctx.Value(keyPrincipal).(*principal)
	return p
}

// withAuth attaches a principal when a valid session cookie or bearer token is present.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p *principal
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			tok, err := s.db.TokenBySecret(r.Context(), strings.TrimPrefix(a, "Bearer "))
			if err != nil {
				problem(w, r, 401, "unauthorized", "Invalid or expired token", "")
				return
			}
			p = &principal{UserID: tok.UserID, Token: tok}
		} else if c, err := r.Cookie(sessionCookie); err == nil {
			if ses, err := s.db.SessionByToken(r.Context(), c.Value); err == nil {
				p = &principal{UserID: ses.UserID, SessionID: ses.ID, CSRF: ses.CSRF}
			}
		}
		if p != nil {
			r = r.WithContext(context.WithValue(r.Context(), keyPrincipal, p))
		}
		next.ServeHTTP(w, r)
	})
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// authed requires a user; cookie sessions must send X-CSRF-Token on mutations.
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := getPrincipal(r.Context())
		if p == nil {
			problem(w, r, 401, "unauthorized", "Sign in required", "")
			return
		}
		if p.Token == nil && unsafeMethod(r.Method) && r.Header.Get("X-CSRF-Token") != p.CSRF {
			problem(w, r, 403, "csrf", "Missing or invalid CSRF token", "")
			return
		}
		h(w, r)
	}
}

// scopeRoles lists the minimum role allowing each scope for cookie sessions.
var scopeAllowed = func(role, scope string) bool {
	switch {
	case scope == "" || strings.HasSuffix(scope, ":read"):
		return store.RoleRank[role] >= 1
	case scope == "reviews:write":
		return role == "reviewer" || store.RoleRank[role] >= store.RoleRank["admin"]
	case scope == "fleet:write":
		return role == "fleet_operator" || store.RoleRank[role] >= store.RoleRank["admin"]
	case scope == "admin":
		return store.RoleRank[role] >= store.RoleRank["admin"]
	default: // specs:write, builds:write
		return store.RoleRank[role] >= store.RoleRank["builder"]
	}
}

// org wraps org-scoped handlers: membership + role/scope checks.
func (s *Server) org(minRole, scope string, h http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		p := getPrincipal(r.Context())
		orgID := r.PathValue("org")
		if p.Token != nil {
			if p.Token.OrgID != orgID {
				problem(w, r, 404, "not-found", "Not found", "")
				return
			}
			if scope != "" && !hasScope(p.Token.Scopes, scope) {
				problem(w, r, 403, "forbidden", "Token lacks scope "+scope, "")
				return
			}
		}
		role, err := s.db.Role(r.Context(), orgID, p.UserID)
		if err != nil {
			problem(w, r, 404, "not-found", "Not found", "") // never reveal other orgs
			return
		}
		special := (scope == "reviews:write" || scope == "fleet:write") && scopeAllowed(role, scope)
		if store.RoleRank[role] < store.RoleRank[minRole] && !special {
			problem(w, r, 403, "forbidden", "Your role does not allow this", "")
			return
		}
		if scope != "" && !scopeAllowed(role, scope) {
			problem(w, r, 403, "forbidden", "Your role does not allow this", "")
			return
		}
		p.OrgID, p.Role = orgID, role
		h(w, r)
	})
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want || s == "admin" || (strings.HasSuffix(want, ":read") && s == strings.TrimSuffix(want, ":read")+":write") {
			return true
		}
	}
	return false
}

func (s *Server) setSessionCookie(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: secret, Path: "/", HttpOnly: true, Secure: s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
}

// ------------------------------------------------------------- email login

func randomCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

func validEmail(e string) (string, bool) {
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != strings.TrimSpace(e) || len(e) > 254 {
		return "", false
	}
	return strings.ToLower(a.Address), true
}

func (s *Server) authEmailStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	email, ok := validEmail(req.Email)
	if !ok {
		problem(w, r, 422, "invalid-email", "Enter a valid email address", "")
		return
	}
	if ok, _ := s.limiter.Allow("login:" + email); !ok {
		problem(w, r, 429, "rate-limited", "Too many login attempts", "")
		return
	}
	code := randomCode()
	if err := s.db.CreateLoginCode(r.Context(), email, code, 10*time.Minute); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.mailer.Send(r.Context(), email, "Your Foundry sign-in code", "Your code is "+code+". It expires in 10 minutes."); err != nil {
		s.log.Error("mail", "err", err)
	}
	resp := map[string]any{"sent": true}
	if s.cfg.DevLogin {
		resp["devCode"] = code
	}
	writeJSON(w, 200, resp)
}

func (s *Server) authEmailVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
		Name  string `json:"name,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	email, ok := validEmail(req.Email)
	if !ok || len(req.Code) != 6 {
		problem(w, r, 422, "invalid-code", "Invalid code", "")
		return
	}
	valid, err := s.db.ConsumeLoginCode(r.Context(), email, req.Code)
	if err != nil && err != store.ErrNotFound {
		s.fail(w, r, err)
		return
	}
	if !valid {
		problem(w, r, 401, "invalid-code", "That code is wrong or expired", "")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.Split(email, "@")[0]
	}
	s.login(w, r, email, name)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, email, name string) {
	u, err := s.db.UpsertUser(r.Context(), email, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ses, secret, err := s.db.CreateSession(r.Context(), u.ID, r.UserAgent(), sessionTTL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setSessionCookie(w, secret)
	s.audit(r.Context(), "", u.ID, "auth.login", u.ID, map[string]string{"method": "email"})
	writeJSON(w, 200, map[string]any{"user": u, "csrf": ses.CSRF})
}

// ------------------------------------------------------------------ OIDC

func (s *Server) authOIDCStart(w http.ResponseWriter, r *http.Request) {
	p := s.cfg.OIDC
	if p == nil {
		problem(w, r, 404, "not-configured", "Single sign-on is not configured", "")
		return
	}
	if err := p.Discover(r.Context()); err != nil {
		problem(w, r, 502, "idp-unavailable", "Identity provider unavailable", err.Error())
		return
	}
	redirect := r.URL.Query().Get("redirect")
	if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}
	state, nonce := oidc.RandomString(), oidc.RandomString()
	verifier, challenge := oidc.PKCE()
	if err := s.db.PutOIDCState(r.Context(), state, verifier, nonce, redirect, 10*time.Minute); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, p.AuthURL(state, nonce, challenge), http.StatusFound)
}

func (s *Server) authOIDCCallback(w http.ResponseWriter, r *http.Request) {
	p := s.cfg.OIDC
	if p == nil {
		problem(w, r, 404, "not-configured", "Single sign-on is not configured", "")
		return
	}
	q := r.URL.Query()
	verifier, nonce, redirect, err := s.db.TakeOIDCState(r.Context(), q.Get("state"))
	if err != nil {
		problem(w, r, 400, "bad-state", "Sign-in expired, please try again", "")
		return
	}
	claims, err := p.Exchange(r.Context(), q.Get("code"), verifier, nonce)
	if err != nil {
		problem(w, r, 401, "oidc-failed", "Single sign-on failed", err.Error())
		return
	}
	if claims.Email == "" || !claims.EmailVerified {
		problem(w, r, 403, "email-unverified", "Your identity provider did not confirm your email address", "")
		return
	}
	u, err := s.db.UpsertUser(r.Context(), strings.ToLower(claims.Email), claims.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.db.LinkOIDC(r.Context(), u.ID, claims.Issuer, claims.Subject)
	_, secret, err := s.db.CreateSession(r.Context(), u.ID, r.UserAgent(), sessionTTL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setSessionCookie(w, secret)
	http.Redirect(w, r, redirect, http.StatusFound)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	if p.SessionID != "" {
		s.db.DeleteSession(r.Context(), p.SessionID)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure})
	w.WriteHeader(204)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	u, err := s.db.GetUser(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ms, err := s.db.Memberships(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if p.Token != nil { // tokens only see their own org
		var only []store.Membership
		for _, m := range ms {
			if m.OrgID == p.Token.OrgID {
				only = append(only, m)
			}
		}
		ms = only
	}
	writeJSON(w, 200, map[string]any{"user": u, "memberships": ms, "csrf": p.CSRF, "sso": s.cfg.OIDC != nil})
}

// ------------------------------------------------------------------- orgs

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	if p.Token != nil {
		problem(w, r, 403, "forbidden", "Tokens cannot create organizations", "")
		return
	}
	var req struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 || !slugRe.MatchString(req.Slug) {
		problem(w, r, 422, "invalid", "Enter a name and a slug (lowercase letters, digits, dashes)", "")
		return
	}
	o, err := s.db.CreateOrg(r.Context(), req.Name, req.Slug, "pilot")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.db.AddMember(r.Context(), o.ID, p.UserID, "owner"); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), o.ID, p.UserID, "org.create", o.ID, nil)
	writeJSON(w, 201, o)
}

func (s *Server) getOrg(w http.ResponseWriter, r *http.Request) {
	o, err := s.db.GetOrg(r.Context(), getPrincipal(r.Context()).OrgID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, o)
}

// patchOrg changes org settings. Enabling expert content (custom packages,
// scripts, overlays) is an owner decision made in a browser session, never
// with an API token, and is audited.
func (s *Server) patchOrg(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	if p.Token != nil {
		problem(w, r, 403, "forbidden", "Organization settings cannot be changed with API tokens", "")
		return
	}
	var req struct {
		ExpertAllowed *bool `json:"expertAllowed"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ExpertAllowed != nil {
		if err := s.db.SetOrgExpert(r.Context(), p.OrgID, *req.ExpertAllowed); err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r.Context(), p.OrgID, p.UserID, "org.expert", p.OrgID, map[string]string{"allowed": strconv.FormatBool(*req.ExpertAllowed)})
	}
	o, err := s.db.GetOrg(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, o)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Pool.Query(r.Context(), `SELECT u.id, u.email, u.display_name, m.role FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.org_id=$1 ORDER BY u.email`,
		getPrincipal(r.Context()).OrgID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	type member struct {
		ID    string `json:"userId"`
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	out := []member{}
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role); err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, m)
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	email, ok := validEmail(req.Email)
	if _, known := store.RoleRank[req.Role]; !ok || !known {
		problem(w, r, 422, "invalid", "Valid email and role required", "")
		return
	}
	if req.Role == "owner" && p.Role != "owner" {
		problem(w, r, 403, "forbidden", "Only owners can add owners", "")
		return
	}
	u, err := s.db.UpsertUser(r.Context(), email, strings.Split(email, "@")[0])
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.db.AddMember(r.Context(), p.OrgID, u.ID, req.Role); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "org.member.add", u.ID, map[string]string{"role": req.Role})
	writeJSON(w, 201, map[string]string{"userId": u.ID, "role": req.Role})
}

var validScopes = map[string]bool{"specs:read": true, "specs:write": true, "builds:read": true, "builds:write": true,
	"fleet:read": true, "fleet:write": true, "reviews:write": true, "admin": true}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresIn int      `json:"expiresInDays"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || len(req.Scopes) == 0 {
		problem(w, r, 422, "invalid", "Name and scopes required", "")
		return
	}
	for _, sc := range req.Scopes {
		if !validScopes[sc] || !scopeAllowed(p.Role, sc) {
			problem(w, r, 403, "forbidden", "You cannot grant scope "+sc, "")
			return
		}
	}
	if req.ExpiresIn <= 0 || req.ExpiresIn > 365 {
		req.ExpiresIn = 90
	}
	tok, secret, err := s.db.CreateToken(r.Context(), p.OrgID, p.UserID, req.Name, req.Scopes, time.Duration(req.ExpiresIn)*24*time.Hour)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "token.create", tok.ID, map[string]any{"scopes": req.Scopes})
	writeJSON(w, 201, map[string]any{"token": tok, "secret": secret})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	if err := s.db.RevokeToken(r.Context(), p.OrgID, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "token.revoke", r.PathValue("id"), nil)
	w.WriteHeader(204)
}
