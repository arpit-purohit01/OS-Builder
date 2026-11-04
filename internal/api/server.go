// Package api is the public REST API (docs/context/api.md): auth, orgs,
// projects, specs, catalog, assets, builds, artifacts, reviews, releases and
// fleet. All errors are RFC 9457 problem documents.
package api

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/ids"
	"foundry.dev/foundry/internal/mail"
	"foundry.dev/foundry/internal/metrics"
	"foundry.dev/foundry/internal/oidc"
	"foundry.dev/foundry/internal/sched"
	"foundry.dev/foundry/internal/store"
)

// Config for the API server.
type Config struct {
	BaseURL      string // public URL, e.g. https://foundry.example.com
	CookieSecure bool
	DevLogin     bool // return login codes in API responses (development only)
	OIDC         *oidc.Provider
	Platform     compiler.Platform
	WebDir       string // built SPA (web/dist); empty disables static serving
	RatePerSec   float64
	RateBurst    int
	// EnvelopeKey signs build job envelopes; workers verify them.
	EnvelopeKey ed25519.PrivateKey
	// EnvelopeTTL bounds how long a queued build stays buildable (default 7 days).
	EnvelopeTTL time.Duration
	// PreviewURL/PreviewToken reach previewd (live previews); empty disables.
	PreviewURL string
	// PresignDownloads redirects artifact downloads to presigned URLs when the
	// store supports it (S3); the endpoint must be reachable by clients.
	PresignDownloads bool
	PreviewToken     string
}

// Server holds dependencies.
type Server struct {
	cfg     Config
	db      *store.DB
	blobs   blob.Store
	signer  *blob.Signer
	mailer  mail.Mailer
	log     *slog.Logger
	metrics *metrics.Registry
	limiter *sched.GCRA
	catMu   sync.RWMutex
	cat     *catalog.Catalog
	hub     *logHub
	hooks   Hooks
}

// Hooks lets other services plug into API events (e.g. scheduler wake-ups).
type Hooks struct {
	BuildCreated func(buildID string)
}

// New creates a server.
func New(cfg Config, db *store.DB, blobs blob.Store, signer *blob.Signer, cat *catalog.Catalog, m mail.Mailer, log *slog.Logger) *Server {
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec, cfg.RateBurst = 20, 60
	}
	return &Server{cfg: cfg, db: db, blobs: blobs, signer: signer, cat: cat, mailer: m, log: log,
		metrics: metrics.Default, limiter: sched.NewGCRA(cfg.RatePerSec, cfg.RateBurst), hub: newLogHub()}
}

// SetHooks registers event hooks.
func (s *Server) SetHooks(h Hooks) { s.hooks = h }

// Catalog returns the current catalog.
func (s *Server) Catalog() *catalog.Catalog {
	s.catMu.RLock()
	defer s.catMu.RUnlock()
	return s.cat
}

// SetCatalog swaps the catalog (hot reload).
func (s *Server) SetCatalog(c *catalog.Catalog) {
	s.catMu.Lock()
	s.cat = c
	s.catMu.Unlock()
}

// Handler builds the router with middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	var h http.Handler = mux
	h = s.withAuth(h)
	h = s.withRateLimit(h)
	h = s.withSecurityHeaders(h)
	h = s.withLogging(h)
	h = s.withRecover(h)
	return h
}

func (s *Server) routes(mux *http.ServeMux) {
	// health & metrics
	s.route(mux, "GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.route(mux, "GET /readyz", s.ready)
	mux.Handle("GET /metrics", s.metrics.Handler())

	// auth
	s.route(mux, "POST /v1/auth/email/start", s.authEmailStart)
	s.route(mux, "POST /v1/auth/email/verify", s.authEmailVerify)
	s.route(mux, "GET /v1/auth/oidc/start", s.authOIDCStart)
	s.route(mux, "GET /v1/auth/oidc/callback", s.authOIDCCallback)
	s.route(mux, "POST /v1/auth/logout", s.authed(s.authLogout))
	s.route(mux, "GET /v1/me", s.authed(s.me))

	// orgs
	s.route(mux, "POST /v1/orgs", s.authed(s.createOrg))
	s.route(mux, "GET /v1/orgs/{org}", s.org("viewer", "", s.getOrg))
	s.route(mux, "POST /v1/orgs/{org}/builds/{build}/preview", s.org("builder", "builds:write", s.startPreview))
	s.route(mux, "GET /v1/orgs/{org}/previews/{id}", s.org("viewer", "builds:read", s.getPreview))
	s.route(mux, "DELETE /v1/orgs/{org}/previews/{id}", s.org("builder", "builds:write", s.stopPreview))
	s.route(mux, "GET /v1/orgs/{org}/previews/{id}/ws", s.org("viewer", "builds:read", s.previewWS))
	s.route(mux, "PATCH /v1/orgs/{org}", s.org("owner", "admin", s.patchOrg))
	s.route(mux, "GET /v1/orgs/{org}/members", s.org("viewer", "", s.listMembers))
	s.route(mux, "POST /v1/orgs/{org}/members", s.org("admin", "admin", s.addMember))
	s.route(mux, "POST /v1/orgs/{org}/tokens", s.org("builder", "admin", s.createToken))
	s.route(mux, "DELETE /v1/orgs/{org}/tokens/{id}", s.org("admin", "admin", s.revokeToken))

	// catalog & schema (public metadata)
	s.route(mux, "GET /v1/catalog", s.getCatalog)
	s.route(mux, "GET /v1/catalog/{version}/cnf", s.getCNF)
	s.route(mux, "GET /v1/schema/osspec", s.getSchema)

	// catalog versions (Phase 2)
	s.route(mux, "GET /v1/orgs/{org}/catalog/versions", s.org("viewer", "specs:read", s.getCatalogVersions))
	s.route(mux, "GET /v1/orgs/{org}/catalog/versions/{version}", s.org("viewer", "specs:read", s.getCatalogVersion))
	s.route(mux, "POST /v1/orgs/{org}/catalog/versions", s.org("builder", "specs:write", s.createCatalogVersion))
	s.route(mux, "POST /v1/orgs/{org}/catalog/versions/{version}/default", s.org("admin", "admin", s.setDefaultCatalogVersion))
	s.route(mux, "GET /v1/orgs/{org}/catalog/versions/{version}/cnf", s.org("viewer", "specs:read", s.getCNFForVersion))

	// projects & specs
	s.route(mux, "GET /v1/orgs/{org}/projects", s.org("viewer", "specs:read", s.listProjects))
	s.route(mux, "POST /v1/orgs/{org}/projects", s.org("builder", "specs:write", s.createProject))
	s.route(mux, "GET /v1/orgs/{org}/projects/{project}", s.org("viewer", "specs:read", s.getProject))
	s.route(mux, "GET /v1/orgs/{org}/projects/{project}/specs", s.org("viewer", "specs:read", s.listSpecs))
	s.route(mux, "POST /v1/orgs/{org}/projects/{project}/specs", s.org("builder", "specs:write", s.createSpec))
	s.route(mux, "GET /v1/orgs/{org}/specs/{spec}", s.org("viewer", "specs:read", s.getSpec))
	s.route(mux, "GET /v1/orgs/{org}/specs/{spec}/diff/{other}", s.org("viewer", "specs:read", s.diffSpecs))
	s.route(mux, "POST /v1/orgs/{org}/specs/validate", s.org("viewer", "specs:read", s.validateSpec))

	// assets
	s.route(mux, "POST /v1/orgs/{org}/assets/uploads", s.org("builder", "specs:write", s.startUpload))
	s.route(mux, "PUT /v1/uploads/{id}", s.putUpload) // authorized by signed URL
	s.route(mux, "GET /v1/orgs/{org}/assets/{digest}", s.org("viewer", "specs:read", s.getAsset))

	// builds
	s.route(mux, "POST /v1/orgs/{org}/builds", s.org("builder", "builds:write", s.createBuild))
	s.route(mux, "GET /v1/orgs/{org}/builds", s.org("viewer", "builds:read", s.listBuilds))
	s.route(mux, "GET /v1/orgs/{org}/builds/{build}", s.org("viewer", "builds:read", s.getBuild))
	s.route(mux, "POST /v1/orgs/{org}/builds/{build}/cancel", s.org("builder", "builds:write", s.cancelBuild))
	s.route(mux, "GET /v1/orgs/{org}/builds/{build}/logs", s.org("viewer", "builds:read", s.buildLogs))
	s.route(mux, "GET /v1/orgs/{org}/builds/{build}/artifacts", s.org("viewer", "builds:read", s.buildArtifacts))

	// blobs (signed URLs)
	s.route(mux, "GET /v1/blobs/{digest}", s.getBlob)
	s.route(mux, "HEAD /v1/blobs/{digest}", s.getBlob)

	s.extraRoutes(mux)

	if s.cfg.WebDir != "" {
		mux.Handle("GET /", spa(s.cfg.WebDir))
	}
}

// extraRoutes is extended by later-phase files (reviews, releases, fleet).
var extraRouteFns []func(s *Server, mux *http.ServeMux)

func (s *Server) extraRoutes(mux *http.ServeMux) {
	for _, f := range extraRouteFns {
		f(s, mux)
	}
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Pool.Ping(ctx); err != nil {
		problem(w, r, 503, "not-ready", "Database unavailable", err.Error())
		return
	}
	w.Write([]byte("ready"))
}

// ------------------------------------------------------------- middleware

type ctxKey int

const (
	keyPrincipal ctxKey = iota
	keyRequestID
)

func requestID(ctx context.Context) string {
	v, _ := ctx.Value(keyRequestID).(string)
	return v
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets WebSocket upgrades (live preview) take over the connection;
// without it the type assertion in ws.Upgrade fails behind this wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("api: connection does not support hijacking")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if rid == "" || len(rid) > 64 {
			rid = ids.New("req")
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), keyRequestID, rid)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		dur := time.Since(start)
		s.metrics.Observe("foundry_http_request_seconds", "HTTP request latency", metrics.DefBuckets, dur.Seconds(), "route", route, "code", strconv.Itoa(sw.status))
		if !strings.HasPrefix(r.URL.Path, "/healthz") && !strings.HasPrefix(r.URL.Path, "/metrics") {
			s.log.Info("http", "method", r.Method, "route", route, "status", sw.status, "bytes", sw.bytes, "dur_ms", dur.Milliseconds(), "rid", rid)
		}
	})
}

func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.log.Error("panic", "err", p, "path", r.URL.Path)
				problem(w, r, 500, "internal", "Internal error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; connect-src 'self'; worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		key := "ip:" + clientIP(r)
		if a := r.Header.Get("Authorization"); a != "" {
			sum := sha256.Sum256([]byte(a))
			key = "tok:" + base64.RawURLEncoding.EncodeToString(sum[:8])
		} else if c, err := r.Cookie(sessionCookie); err == nil {
			sum := sha256.Sum256([]byte(c.Value))
			key = "ses:" + base64.RawURLEncoding.EncodeToString(sum[:8])
		}
		ok, retry := s.limiter.Allow(key)
		w.Header().Set("RateLimit-Limit", strconv.Itoa(s.cfg.RateBurst))
		if !ok {
			secs := int(retry.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			w.Header().Set("RateLimit-Reset", strconv.Itoa(secs))
			problem(w, r, 429, "rate-limited", "Too many requests", fmt.Sprintf("Retry in %d s.", secs))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// -------------------------------------------------------------- utilities

// MaxJSONBody bounds request bodies for JSON endpoints.
const MaxJSONBody = 2 << 20

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		problem(w, r, 400, "bad-request", "Invalid JSON body", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func readAllLimited(r io.Reader, n int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > n {
		return nil, errors.New("body too large")
	}
	return b, nil
}

// cursor encoding (opaque to clients)
func encodeCursor(c *store.Cursor) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*store.Cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c store.Cursor
	return &c, json.Unmarshal(b, &c)
}

func limitParam(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

var (
	routeMu    sync.Mutex
	registered = map[string]bool{} // every route pattern ever registered (test coverage check)
	routeHook  func(pattern string)
)

// route registers a handler and reports each request's pattern to routeHook
// (set by tests to prove that every endpoint is exercised).
func (s *Server) route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	routeMu.Lock()
	registered[pattern] = true
	routeMu.Unlock()
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		routeMu.Lock()
		hook := routeHook
		routeMu.Unlock()
		if hook != nil {
			hook(pattern)
		}
		h(w, r)
	})
}
