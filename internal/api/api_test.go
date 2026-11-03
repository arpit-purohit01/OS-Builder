package api

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/mail"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/testdb"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	api    *Server
	db     *store.DB
	blobs  *blob.FS
	builds []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testdb.New(t)
	cat, err := catalog.LoadDir("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureCatalogVersion(t.Context(), cat.Version, []byte("cnf"), true); err != nil {
		t.Fatal(err)
	}
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, blobs: fs}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	signer := &blob.Signer{Key: []byte("test-key-0123456789abcdef")}
	e.api = New(Config{DevLogin: true, Platform: compiler.Platform{Tenant: "t"}, RatePerSec: 1000, RateBurst: 1000},
		db, fs, signer, cat, mail.LogMailer{Log: log}, log)
	e.api.SetHooks(Hooks{BuildCreated: func(id string) { e.builds = append(e.builds, id) }})
	e.srv = httptest.NewServer(e.api.Handler())
	signer.BaseURL = e.srv.URL
	t.Cleanup(e.srv.Close)
	return e
}

// client is a browser-like session (cookie jar + CSRF header).
type client struct {
	e    *env
	hc   *http.Client
	csrf string
	tok  string
}

func (e *env) anon() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, hc: &http.Client{Jar: jar}}
}

func (e *env) login(email string) *client {
	c := e.anon()
	var start struct{ DevCode string }
	c.must("POST", "/v1/auth/email/start", map[string]string{"email": email}, 200, &start)
	if len(start.DevCode) != 6 {
		e.t.Fatalf("no dev code: %+v", start)
	}
	var v struct{ CSRF string }
	c.must("POST", "/v1/auth/email/verify", map[string]string{"email": email, "code": start.DevCode}, 200, &v)
	c.csrf = v.CSRF
	return c
}

func (c *client) do(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	c.e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.e.srv.URL + path
	}
	req, _ := http.NewRequest(method, u, rd)
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (c *client) must(method, path string, body any, want int, out any) []byte {
	c.e.t.Helper()
	resp, b := c.do(method, path, body, nil)
	if resp.StatusCode != want {
		c.e.t.Fatalf("%s %s: status %d want %d: %s", method, path, resp.StatusCode, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.e.t.Fatalf("decode %s: %v", b, err)
		}
	}
	return b
}

type orgResp struct{ ID string }
type projResp struct{ ID, OSID string }

func setupOrg(t *testing.T, e *env, email, slug string) (*client, string, string) {
	c := e.login(email)
	var o orgResp
	c.must("POST", "/v1/orgs", map[string]string{"name": "Org " + slug, "slug": slug}, 201, &o)
	var p projResp
	c.must("POST", "/v1/orgs/"+o.ID+"/projects", map[string]string{"name": "CI", "osId": "ci-server"}, 201, &p)
	return c, o.ID, p.ID
}

func vmSpec(t *testing.T) []byte {
	b, err := os.ReadFile("../../test/golden/specs/vm-server.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAuthFlow(t *testing.T) {
	e := newEnv(t)
	anon := e.anon()
	if r, _ := anon.do("GET", "/v1/me", nil, nil); r.StatusCode != 401 {
		t.Fatalf("anon /me: %d", r.StatusCode)
	}
	if r, b := anon.do("POST", "/v1/auth/email/start", map[string]string{"email": "not-an-email"}, nil); r.StatusCode != 422 ||
		r.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("bad email: %d %s %s", r.StatusCode, r.Header.Get("Content-Type"), b)
	}
	c := e.login("alice@example.com")
	var me struct {
		User struct{ Email string }
	}
	c.must("GET", "/v1/me", nil, 200, &me)
	if me.User.Email != "alice@example.com" {
		t.Fatalf("me: %+v", me)
	}
	// CSRF: mutation without header is refused.
	saved := c.csrf
	c.csrf = ""
	if r, _ := c.do("POST", "/v1/orgs", map[string]string{"name": "X", "slug": "xorg"}, nil); r.StatusCode != 403 {
		t.Fatalf("missing csrf: %d", r.StatusCode)
	}
	c.csrf = saved
	// Wrong code is rejected and codes are single use.
	a := e.anon()
	var st struct{ DevCode string }
	a.must("POST", "/v1/auth/email/start", map[string]string{"email": "bob@example.com"}, 200, &st)
	wrong := "000000"
	if st.DevCode == wrong {
		wrong = "111111"
	}
	a.must("POST", "/v1/auth/email/verify", map[string]string{"email": "bob@example.com", "code": wrong}, 401, nil)
	a.must("POST", "/v1/auth/email/verify", map[string]string{"email": "bob@example.com", "code": st.DevCode}, 200, nil)
	a.must("POST", "/v1/auth/email/verify", map[string]string{"email": "bob@example.com", "code": st.DevCode}, 401, nil)
	// Logout invalidates the session.
	c.must("POST", "/v1/auth/logout", nil, 204, nil)
	if r, _ := c.do("GET", "/v1/me", nil, nil); r.StatusCode != 401 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
}

func TestSecurityHeadersAndHealth(t *testing.T) {
	e := newEnv(t)
	r, b := e.anon().do("GET", "/healthz", nil, nil)
	if r.StatusCode != 200 || string(b) != "ok" {
		t.Fatal("healthz")
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "X-Request-Id"} {
		if r.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if r, _ := e.anon().do("GET", "/readyz", nil, nil); r.StatusCode != 200 {
		t.Fatal("readyz")
	}
	if r, b := e.anon().do("GET", "/metrics", nil, nil); r.StatusCode != 200 || !strings.Contains(string(b), "foundry_http_request_seconds") {
		t.Fatalf("metrics: %s", b)
	}
}

func TestTenancyIsolation(t *testing.T) {
	e := newEnv(t)
	a, orgA, projA := setupOrg(t, e, "a@example.com", "org-a")
	b, orgB, _ := setupOrg(t, e, "b@example.com", "org-b")
	// b cannot see a's org, project, or specs: always 404, never 403.
	for _, p := range []string{"/v1/orgs/" + orgA, "/v1/orgs/" + orgA + "/projects", "/v1/orgs/" + orgA + "/projects/" + projA} {
		if r, _ := b.do("GET", p, nil, nil); r.StatusCode != 404 {
			t.Errorf("cross-tenant %s: %d", p, r.StatusCode)
		}
	}
	// IDs from another org are not found even through b's own org path.
	if r, _ := b.do("GET", "/v1/orgs/"+orgB+"/projects/"+projA, nil, nil); r.StatusCode != 404 {
		t.Errorf("foreign project via own org: %d", r.StatusCode)
	}
	// Roles: a viewer cannot create projects.
	a.must("POST", "/v1/orgs/"+orgA+"/members", map[string]string{"email": "v@example.com", "role": "viewer"}, 201, nil)
	v := e.login("v@example.com")
	v.must("GET", "/v1/orgs/"+orgA+"/projects", nil, 200, nil)
	if r, _ := v.do("POST", "/v1/orgs/"+orgA+"/projects", map[string]string{"name": "x", "osId": "xyz"}, nil); r.StatusCode != 403 {
		t.Errorf("viewer create: %d", r.StatusCode)
	}
	// Tokens: scoped, org-bound, and cannot exceed the creator's role.
	if r, _ := v.do("POST", "/v1/orgs/"+orgA+"/tokens", map[string]any{"name": "t", "scopes": []string{"specs:write"}}, nil); r.StatusCode != 403 {
		t.Errorf("viewer token: %d", r.StatusCode)
	}
	var tok struct{ Secret string }
	a.must("POST", "/v1/orgs/"+orgA+"/tokens", map[string]any{"name": "ci", "scopes": []string{"specs:read"}}, 201, &tok)
	if !strings.HasPrefix(tok.Secret, "fdy_pat_") {
		t.Fatalf("token: %+v", tok)
	}
	tc := &client{e: e, hc: http.DefaultClient, tok: tok.Secret}
	tc.must("GET", "/v1/orgs/"+orgA+"/projects", nil, 200, nil)
	if r, _ := tc.do("POST", "/v1/orgs/"+orgA+"/projects", map[string]string{"name": "x", "osId": "xyz"}, nil); r.StatusCode != 403 {
		t.Errorf("read token write: %d", r.StatusCode)
	}
	if r, _ := tc.do("GET", "/v1/orgs/"+orgB+"/projects", nil, nil); r.StatusCode != 404 {
		t.Errorf("token other org: %d", r.StatusCode)
	}
	if r, _ := (&client{e: e, hc: http.DefaultClient, tok: "fdy_pat_bogus"}).do("GET", "/v1/me", nil, nil); r.StatusCode != 401 {
		t.Errorf("bogus token: %d", r.StatusCode)
	}
}

func TestSpecLifecycle(t *testing.T) {
	e := newEnv(t)
	c, org, proj := setupOrg(t, e, "s@example.com", "spec-org")
	base := "/v1/orgs/" + org
	// validate: OK
	var val struct {
		Valid  bool
		Digest string
	}
	c.must("POST", base+"/specs/validate", vmSpec(t), 200, &val)
	if !val.Valid || !strings.HasPrefix(val.Digest, "sha256:") {
		t.Fatalf("validate: %+v", val)
	}
	// validate: schema error is reported as a problem with field errors
	r, b := c.do("POST", base+"/specs/validate", "apiVersion: foundry.dev/v1alpha1\nkind: OSSpec\nmetadata: {name: X}\n", nil)
	if r.StatusCode != 422 || !strings.Contains(string(b), `"errors"`) {
		t.Fatalf("invalid spec: %d %s", r.StatusCode, b)
	}
	// catalog conflict: kiosk profile with a desktop app → explained by the solver
	bad := strings.Replace(string(vmSpec(t)), "profile: server", "profile: kiosk-cage", 1) + "apps: {catalog: [firefox]}\n"
	r, b = c.do("POST", base+"/specs/validate", bad, nil)
	if r.StatusCode != 422 {
		t.Fatalf("conflict: %d %s", r.StatusCode, b)
	}
	// create v1
	var s1 struct {
		SpecVersion struct{ ID, Digest string }
		Created     bool
	}
	c.must("POST", base+"/projects/"+proj+"/specs", vmSpec(t), 201, &s1)
	// same content → same version (idempotent)
	var again struct {
		SpecVersion struct{ ID string }
		Created     bool
	}
	c.must("POST", base+"/projects/"+proj+"/specs", vmSpec(t), 200, &again)
	if again.Created || again.SpecVersion.ID != s1.SpecVersion.ID {
		t.Fatalf("idempotent spec: %+v vs %+v", again, s1)
	}
	// v2 via JSON patch on the parent
	var s2 struct {
		SpecVersion struct {
			ID       string
			ParentID *string
			Patch    json.RawMessage `json:"patchFromParent"`
		}
	}
	c.must("POST", base+"/projects/"+proj+"/specs", map[string]any{"parent": s1.SpecVersion.ID,
		"patch": []map[string]any{{"op": "replace", "path": "/metadata/version", "value": "1.0.1"}}}, 201, &s2)
	if s2.SpecVersion.ParentID == nil || *s2.SpecVersion.ParentID != s1.SpecVersion.ID || !strings.Contains(string(s2.SpecVersion.Patch), "1.0.1") {
		t.Fatalf("patched spec: %+v", s2)
	}
	// diff
	var d struct{ Patch []map[string]any }
	c.must("GET", base+"/specs/"+s1.SpecVersion.ID+"/diff/"+s2.SpecVersion.ID, nil, 200, &d)
	if len(d.Patch) != 1 || d.Patch[0]["path"] != "/metadata/version" {
		t.Fatalf("diff: %+v", d)
	}
	// yaml export round-trips through validation
	_, y := c.do("GET", base+"/specs/"+s2.SpecVersion.ID+"?format=yaml", nil, nil)
	c.must("POST", base+"/specs/validate", y, 200, nil)
	// osId must match the project
	other := strings.Replace(string(vmSpec(t)), "name: ci-server", "name: other-os", 1)
	r, b = c.do("POST", base+"/projects/"+proj+"/specs", other, nil)
	if r.StatusCode != 422 || !strings.Contains(string(b), "PRJ-001") {
		t.Fatalf("osId mismatch: %d %s", r.StatusCode, b)
	}
	// list, newest first
	var list struct{ Items []struct{ ID string } }
	c.must("GET", base+"/projects/"+proj+"/specs", nil, 200, &list)
	if len(list.Items) != 2 || list.Items[0].ID != s2.SpecVersion.ID {
		t.Fatalf("list: %+v", list)
	}
	// expert specs are refused by default org policy
	expert := string(vmSpec(t)) + "expert: {postinstScripts: [\"sha256:" + strings.Repeat("ab", 32) + "\"]}\n"
	r, b = c.do("POST", base+"/specs/validate", expert, nil)
	if r.StatusCode != 403 || !strings.Contains(string(b), "POL-002") {
		t.Fatalf("expert policy: %d %s", r.StatusCode, b)
	}
}

func TestCatalogEndpoints(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	r, b := c.do("GET", "/v1/catalog?base=ubuntu-24.04", nil, nil)
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	var cat struct {
		Version string
		Options []struct {
			ID        string
			Available bool
		}
	}
	json.Unmarshal(b, &cat)
	if cat.Version != e.api.Catalog().Version || len(cat.Options) < 20 {
		t.Fatalf("catalog: %s %d", cat.Version, len(cat.Options))
	}
	if r2, _ := c.do("GET", "/v1/catalog?base=ubuntu-24.04", nil, map[string]string{"If-None-Match": r.Header.Get("ETag")}); r2.StatusCode != 304 {
		t.Fatalf("etag: %d", r2.StatusCode)
	}
	r, b = c.do("GET", "/v1/catalog/"+cat.Version+"/cnf", nil, nil)
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") || !strings.Contains(string(b), "Vars") && !strings.Contains(string(b), "vars") {
		t.Fatalf("cnf: %d %s", r.StatusCode, b[:min(len(b), 200)])
	}
	if r, _ := c.do("GET", "/v1/catalog/deadbeef/cnf", nil, nil); r.StatusCode != 404 {
		t.Fatal("unknown cnf version")
	}
	if r, _ := c.do("GET", "/v1/schema/osspec", nil, nil); r.StatusCode != 200 {
		t.Fatal("schema")
	}
}

func pngBytes(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for x := 0; x < 64; x++ {
		img.Set(x, x%32, color.RGBA{200, 50, 50, 255})
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func doUpload(t *testing.T, c *client, org, kind, media string, data []byte) (int, []byte, string) {
	sum := sha256.Sum256(data)
	var st struct{ UploadID, URL, Digest string }
	c.must("POST", "/v1/orgs/"+org+"/assets/uploads", map[string]any{"kind": kind, "mediaType": media, "size": len(data), "sha256": hex.EncodeToString(sum[:])}, 201, &st)
	r, b := c.do("PUT", st.URL, data, nil)
	return r.StatusCode, b, st.Digest
}

func TestUploadsAndBlobs(t *testing.T) {
	e := newEnv(t)
	c, org, _ := setupOrg(t, e, "u@example.com", "up-org")
	data := pngBytes(t)
	code, b, digest := doUpload(t, c, org, "image", "image/png", data)
	if code != 201 || !strings.Contains(string(b), `"valid"`) {
		t.Fatalf("upload: %d %s", code, b)
	}
	var ga struct{ URL string }
	c.must("GET", "/v1/orgs/"+org+"/assets/"+digest, nil, 200, &ga)
	r, got := e.anon().do("GET", ga.URL, nil, nil)
	if r.StatusCode != 200 || !bytes.Equal(got, data) {
		t.Fatalf("download: %d", r.StatusCode)
	}
	// Range request
	r, got = e.anon().do("GET", ga.URL, nil, map[string]string{"Range": "bytes=0-7"})
	if r.StatusCode != 206 || !bytes.Equal(got, data[:8]) {
		t.Fatalf("range: %d", r.StatusCode)
	}
	// tampered signature
	if r, _ := e.anon().do("GET", strings.Replace(ga.URL, "sig=", "sig=00", 1), nil, nil); r.StatusCode != 403 {
		t.Fatalf("tampered: %d", r.StatusCode)
	}
	// SVG with script is stored but rejected
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if code, b, _ := doUpload(t, c, org, "image", "image/svg+xml", svg); code != 422 || !strings.Contains(string(b), "UPL-SVG-001") {
		t.Fatalf("svg: %d %s", code, b)
	}
	// bytes that don't match the declared digest
	sum := sha256.Sum256(data)
	var st struct{ URL string }
	c.must("POST", "/v1/orgs/"+org+"/assets/uploads", map[string]any{"kind": "image", "mediaType": "image/png", "size": len(data), "sha256": hex.EncodeToString(sum[:])}, 201, &st)
	evil := append([]byte{}, data...)
	evil[len(evil)-1] ^= 0xff
	if r, _ := c.do("PUT", st.URL, evil, nil); r.StatusCode != 422 {
		t.Fatalf("digest mismatch: %d", r.StatusCode)
	}
	// overlays need expert mode
	if r, _ := c.do("POST", "/v1/orgs/"+org+"/assets/uploads", map[string]any{"kind": "overlay", "mediaType": "application/x-tar", "size": 10, "sha256": strings.Repeat("a", 64)}, nil); r.StatusCode != 403 {
		t.Fatalf("overlay: %d", r.StatusCode)
	}
	// other orgs cannot read the asset record
	b2, orgB, _ := setupOrg(t, e, "u2@example.com", "up-org-b")
	if r, _ := b2.do("GET", "/v1/orgs/"+orgB+"/assets/"+digest, nil, nil); r.StatusCode != 404 {
		t.Fatalf("cross-org asset: %d", r.StatusCode)
	}
}

func TestBuildCreateIdempotentAndLogs(t *testing.T) {
	e := newEnv(t)
	c, org, proj := setupOrg(t, e, "b@example.com", "build-org")
	base := "/v1/orgs/" + org
	var sv struct{ SpecVersion struct{ ID string } }
	c.must("POST", base+"/projects/"+proj+"/specs", vmSpec(t), 201, &sv)
	body := map[string]any{"specVersionId": sv.SpecVersion.ID}
	r, b := c.do("POST", base+"/builds", body, map[string]string{"Idempotency-Key": "k1"})
	if r.StatusCode != 202 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	var cr struct {
		Build struct{ ID, State, TreeDigest string }
	}
	json.Unmarshal(b, &cr)
	if cr.Build.State != "queued" || !strings.HasPrefix(cr.Build.TreeDigest, "sha256:") {
		t.Fatalf("build: %s", b)
	}
	if len(e.builds) != 1 || e.builds[0] != cr.Build.ID {
		t.Fatalf("hook: %v", e.builds)
	}
	// The compiled tree is stored content-addressed.
	if _, err := e.blobs.Stat(t.Context(), cr.Build.TreeDigest); err != nil {
		t.Fatalf("tree blob: %v", err)
	}
	// Replay with the same key returns the same build.
	r2, b2 := c.do("POST", base+"/builds", body, map[string]string{"Idempotency-Key": "k1"})
	var cr2 struct{ Build struct{ ID string } }
	json.Unmarshal(b2, &cr2)
	if r2.StatusCode != 202 || r2.Header.Get("Idempotent-Replayed") != "true" || cr2.Build.ID != cr.Build.ID {
		t.Fatalf("replay: %d %s", r2.StatusCode, b2)
	}
	// Same key, different body → conflict.
	if r3, _ := c.do("POST", base+"/builds", map[string]any{"specVersionId": sv.SpecVersion.ID, "priority": 3}, map[string]string{"Idempotency-Key": "k1"}); r3.StatusCode != 409 {
		t.Fatalf("idem mismatch: %d", r3.StatusCode)
	}
	if len(e.builds) != 1 {
		t.Fatalf("replay must not enqueue: %v", e.builds)
	}
	// list & get
	var list struct{ Items []struct{ ID string } }
	c.must("GET", base+"/builds", nil, 200, &list)
	if len(list.Items) != 1 {
		t.Fatalf("list: %+v", list)
	}
	c.must("GET", base+"/builds/"+cr.Build.ID, nil, 200, nil)
	// logs written by a worker show up in JSON and SSE.
	ctx := t.Context()
	if err := e.db.AppendLogs(ctx, cr.Build.ID, "stdout", []string{"hello", "world"}); err != nil {
		t.Fatal(err)
	}
	var logs struct{ Items []struct{ Line string } }
	c.must("GET", base+"/builds/"+cr.Build.ID+"/logs", nil, 200, &logs)
	if len(logs.Items) != 2 || logs.Items[1].Line != "world" {
		t.Fatalf("logs: %+v", logs)
	}
	// Cancel → terminal; SSE replays logs then ends.
	c.must("POST", base+"/builds/"+cr.Build.ID+"/cancel", nil, 200, nil)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+base+"/builds/"+cr.Build.ID+"/logs", nil)
	req.Header.Set("Accept", "text/event-stream")
	hc := &http.Client{Jar: c.hc.Jar, Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var events []string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: ") {
			events = append(events, strings.TrimPrefix(sc.Text(), "event: "))
		}
	}
	if strings.Join(events, ",") != "log,log,state,end" {
		t.Fatalf("sse events: %v", events)
	}
	// Artifacts list is empty and not downloadable yet.
	var arts struct{ Downloadable bool }
	c.must("GET", base+"/builds/"+cr.Build.ID+"/artifacts", nil, 200, &arts)
	if arts.Downloadable {
		t.Fatal("cancelled build must not be downloadable")
	}
	// Audit chain stays valid across all of the above.
	if n, err := e.db.VerifyAuditChain(ctx); err != nil || n != 0 {
		t.Fatalf("audit: %d %v", n, err)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	e.api.limiter.Rate, e.api.limiter.Burst = 1, 3
	c := e.anon()
	got429 := false
	for i := 0; i < 10; i++ {
		if r, _ := c.do("GET", "/v1/catalog", nil, nil); r.StatusCode == 429 {
			got429 = r.Header.Get("Retry-After") != ""
			break
		}
	}
	if !got429 {
		t.Fatal("expected 429 with Retry-After")
	}
}

func TestReviewTwoPersonRule(t *testing.T) {
	e := newEnv(t)
	owner, org, proj := setupOrg(t, e, "own@example.com", "rev-org")
	base := "/v1/orgs/" + org
	var sv struct{ SpecVersion struct{ ID string } }
	owner.must("POST", base+"/projects/"+proj+"/specs", vmSpec(t), 201, &sv)
	var cr struct{ Build struct{ ID string } }
	owner.must("POST", base+"/builds", map[string]any{"specVersionId": sv.SpecVersion.ID}, 202, &cr)
	id := cr.Build.ID
	ctx := t.Context()
	risk := 70
	for _, st := range [][2]string{{"queued", "leased"}, {"leased", "building"}, {"building", "built"}, {"built", "scanning"}, {"scanning", "scanned"},
		{"scanned", "testing"}, {"testing", "tested"}, {"tested", "awaiting_review"}} {
		o := store.TransitionOpts{}
		if st[1] == "scanned" {
			o.Risk = &risk
		}
		if err := e.db.Transition(ctx, id, st[0], st[1], o); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []string{"r1@example.com", "r2@example.com"} {
		owner.must("POST", base+"/members", map[string]string{"email": m, "role": "reviewer"}, 201, nil)
	}
	owner.must("POST", base+"/members", map[string]string{"email": "dev@example.com", "role": "builder"}, 201, nil)
	r1, r2, dev := e.login("r1@example.com"), e.login("r2@example.com"), e.login("dev@example.com")
	// builders cannot review; the creator cannot review their own build
	if r, _ := dev.do("POST", base+"/builds/"+id+"/reviews", map[string]string{"decision": "approve"}, nil); r.StatusCode != 403 {
		t.Fatalf("builder review: %d", r.StatusCode)
	}
	if r, _ := owner.do("POST", base+"/builds/"+id+"/reviews", map[string]string{"decision": "approve"}, nil); r.StatusCode != 403 {
		t.Fatalf("self review: %d", r.StatusCode)
	}
	var q struct{ Items []struct{ ID string } }
	r1.must("GET", base+"/reviews", nil, 200, &q)
	if len(q.Items) != 1 || q.Items[0].ID != id {
		t.Fatalf("queue %+v", q)
	}
	var sec struct{ ReviewersRequired int }
	r1.must("GET", base+"/builds/"+id+"/security", nil, 200, &sec)
	if sec.ReviewersRequired != 2 {
		t.Fatalf("risk 70 needs two reviewers: %d", sec.ReviewersRequired)
	}
	var out struct{ State string }
	r1.must("POST", base+"/builds/"+id+"/reviews", map[string]string{"decision": "approve"}, 200, &out)
	if out.State != "awaiting_review" {
		t.Fatalf("one approval: %s", out.State)
	}
	if r, _ := r1.do("POST", base+"/builds/"+id+"/reviews", map[string]string{"decision": "approve"}, nil); r.StatusCode != 409 {
		t.Fatalf("double vote: %d", r.StatusCode)
	}
	if r, _ := r2.do("POST", base+"/builds/"+id+"/reviews", map[string]string{"decision": "reject"}, nil); r.StatusCode != 422 {
		t.Fatalf("reject without comment: %d", r.StatusCode)
	}
	r2.must("POST", base+"/builds/"+id+"/reviews", map[string]string{"decision": "approve", "comment": "diff reviewed"}, 200, &out)
	b, _ := e.db.GetBuildAny(ctx, id)
	if out.State != "approved" || b.State != "approved" || b.Tier == nil || *b.Tier != "reviewed" {
		t.Fatalf("after two approvals: %s %s %v", out.State, b.State, b.Tier)
	}
	var n int
	e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind='build.finalize' AND unique_key=$1`, "finalize:"+id).Scan(&n)
	if n != 1 {
		t.Fatalf("finalize job not enqueued: %d", n)
	}
}
