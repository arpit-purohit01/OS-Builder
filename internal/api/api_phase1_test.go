package api

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"foundry.dev/foundry/internal/oidc"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"foundry.dev/foundry/internal/store"
)

func tarWith(t *testing.T, name, body string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	return b.Bytes()
}

func pushSpec(t *testing.T, c *client, org, project string, doc []byte) string {
	var r struct {
		SpecVersion struct{ ID string } `json:"specVersion"`
	}
	resp, b := c.do("POST", "/v1/orgs/"+org+"/projects/"+project+"/specs", doc, map[string]string{"Content-Type": "application/yaml"})
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("push spec: %d %s", resp.StatusCode, b)
	}
	json.Unmarshal(b, &r)
	return r.SpecVersion.ID
}

func problemOf(t *testing.T, b []byte) Problem {
	var p Problem
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("not a problem document: %s", b)
	}
	return p
}

// Phase 1 acceptance 6: an overlay with ../../etc/shadow is rejected with a
// 422 problem response, at upload and when a spec references it.
func TestOverlayTraversalRejected(t *testing.T) {
	e := newEnv(t)
	c, org, project := setupOrg(t, e, "ov@example.com", "ov-org")
	// Expert content is an owner decision, made in a browser session.
	var o struct{ ExpertAllowed bool }
	c.must("PATCH", "/v1/orgs/"+org, map[string]any{"expertAllowed": true}, 200, &o)
	if !o.ExpertAllowed {
		t.Fatal("expert mode not enabled")
	}
	evil := tarWith(t, "../../etc/shadow", "root::0:0:::::\n")
	code, body, digest := doUpload(t, c, org, "overlay", "application/x-tar", evil)
	if code != 422 {
		t.Fatalf("traversal overlay accepted: %d %s", code, body)
	}
	if !strings.Contains(string(body), `"rejected"`) {
		t.Fatalf("validation report: %s", body)
	}
	spec := append(append([]byte{}, vmSpec(t)...), []byte("expert:\n  overlays: [\""+digest+"\"]\n")...)
	sv := pushSpec(t, c, org, project, spec)
	resp, b := c.do("POST", "/v1/orgs/"+org+"/builds", map[string]any{"specVersionId": sv}, nil)
	if resp.StatusCode != 422 || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("build with rejected overlay: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	p := problemOf(t, b)
	if len(p.Errors) == 0 || p.Errors[0].Pointer != "/expert/overlays/0" || p.Errors[0].Code != "AST-404" {
		t.Fatalf("problem: %+v", p)
	}
	// A clean overlay is accepted and the build is queued.
	code, _, good := doUpload(t, c, org, "overlay", "application/x-tar", tarWith(t, "etc/motd", "hello\n"))
	if code != 201 {
		t.Fatalf("clean overlay: %d", code)
	}
	sv = pushSpec(t, c, org, project, append(append([]byte{}, vmSpec(t)...), []byte("expert:\n  overlays: [\""+good+"\"]\n")...))
	c.must("POST", "/v1/orgs/"+org+"/builds", map[string]any{"specVersionId": sv}, 202, nil)
}

func TestOrgSettingsMembersTokens(t *testing.T) {
	e := newEnv(t)
	owner, org, _ := setupOrg(t, e, "own@example.com", "set-org")
	// a builder cannot change org settings
	builder := e.login("bld@example.com")
	owner.must("POST", "/v1/orgs/"+org+"/members", map[string]string{"email": "bld@example.com", "role": "builder"}, 201, nil)
	if r, _ := builder.do("PATCH", "/v1/orgs/"+org, map[string]any{"expertAllowed": true}, nil); r.StatusCode != 403 {
		t.Fatalf("builder changed org settings: %d", r.StatusCode)
	}
	var ms struct {
		Items []struct{ Email, Role string }
	}
	builder.must("GET", "/v1/orgs/"+org+"/members", nil, 200, &ms)
	if len(ms.Items) != 2 {
		t.Fatalf("members: %+v", ms)
	}
	// API tokens: scoped, cannot change org settings, revocable
	var tok struct {
		Token  struct{ ID string }
		Secret string
	}
	owner.must("POST", "/v1/orgs/"+org+"/tokens", map[string]any{"name": "ci", "scopes": []string{"builds:read"}, "expiresInDays": 1}, 201, &tok)
	tc := &client{e: e, hc: &http.Client{}, tok: tok.Secret}
	tc.must("GET", "/v1/orgs/"+org+"/builds", nil, 200, nil)
	if r, _ := tc.do("POST", "/v1/orgs/"+org+"/projects", map[string]string{"name": "x", "osId": "xyz"}, nil); r.StatusCode != 403 {
		t.Fatalf("read-only token created a project: %d", r.StatusCode)
	}
	if r, _ := tc.do("PATCH", "/v1/orgs/"+org, map[string]any{"expertAllowed": true}, nil); r.StatusCode != 403 {
		t.Fatalf("token changed org settings: %d", r.StatusCode)
	}
	owner.must("DELETE", "/v1/orgs/"+org+"/tokens/"+tok.Token.ID, nil, 204, nil)
	if r, _ := tc.do("GET", "/v1/orgs/"+org+"/builds", nil, nil); r.StatusCode != 401 {
		t.Fatalf("revoked token still works: %d", r.StatusCode)
	}
	// the expert toggle is audited
	var n int
	e.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE action='token.revoke' AND org_id=$1`, org).Scan(&n)
	if n != 1 {
		t.Fatalf("audit entries: %d", n)
	}
}

func TestCancelQueuedBuild(t *testing.T) {
	e := newEnv(t)
	c, org, project := setupOrg(t, e, "cx@example.com", "cx-org")
	sv := pushSpec(t, c, org, project, vmSpec(t))
	var bw struct{ Build store.Build }
	c.must("POST", "/v1/orgs/"+org+"/builds", map[string]any{"specVersionId": sv}, 202, &bw)
	b := bw.Build
	c.must("POST", "/v1/orgs/"+org+"/builds/"+b.ID+"/cancel", nil, 200, nil)
	var got struct{ Build store.Build }
	c.must("GET", "/v1/orgs/"+org+"/builds/"+b.ID, nil, 200, &got)
	if got.Build.State != store.StateCancelled && !got.Build.CancelRequested {
		t.Fatalf("state %s", got.Build.State)
	}
	var arts struct {
		Items        []any
		Downloadable bool
	}
	c.must("GET", "/v1/orgs/"+org+"/builds/"+b.ID+"/artifacts", nil, 200, &arts)
	if arts.Downloadable || len(arts.Items) != 0 {
		t.Fatalf("artifacts of a cancelled build: %+v", arts)
	}
}

func TestLayoutAssets(t *testing.T) {
	e := newEnv(t)
	c, org, _ := setupOrg(t, e, "lay@example.com", "lay-org")
	if code, b, _ := doUpload(t, c, org, "layout", "application/json", []byte(`{"version":1,"panels":[{"edge":"top","style":"bar","size":28,"items":["clock"],"exec":"x"}]}`)); code != 422 || !strings.Contains(string(b), "LAYOUT") {
		t.Fatalf("invalid layout: %d %s", code, b)
	}
	if code, b, _ := doUpload(t, c, org, "layout", "application/json", []byte(`{"version":1,"panels":[{"edge":"bottom","style":"dock","size":48,"items":["launcher","tasks"]}]}`)); code != 201 {
		t.Fatalf("valid layout: %d %s", code, b)
	}
}

// fakePreviewd stands in for previewd: sessions plus an echoing RFB stream.
func fakePreviewd(t *testing.T, token string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/sessions":
			json.NewEncoder(w).Encode(map[string]any{"id": "pvw_remote", "expiresAt": time.Now().Add(15 * time.Minute)})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/rfb"):
			c, rw, _ := w.(http.Hijacker).Hijack()
			defer c.Close()
			io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: foundry-rfb\r\nConnection: Upgrade\r\n\r\n")
			io.WriteString(c, "RFB 003.008\n")
			buf := make([]byte, 64)
			n, _ := rw.Read(buf)
			c.Write(bytes.ToUpper(buf[:n]))
		case r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"state": "running"})
		case r.Method == "DELETE":
			w.WriteHeader(204)
		}
	}))
}

func TestLivePreview(t *testing.T) {
	e := newEnv(t)
	c, org, project := setupOrg(t, e, "pv@example.com", "pv-org")
	sv := pushSpec(t, c, org, project, vmSpec(t))
	var bw struct{ Build store.Build }
	c.must("POST", "/v1/orgs/"+org+"/builds", map[string]any{"specVersionId": sv}, 202, &bw)
	b := bw.Build
	start := "/v1/orgs/" + org + "/builds/" + b.ID + "/preview"
	if r, _ := c.do("POST", start, nil, nil); r.StatusCode != 503 {
		t.Fatalf("preview without previewd: %d", r.StatusCode)
	}
	pd := fakePreviewd(t, "preview-token-0123456789")
	defer pd.Close()
	e.api.cfg.PreviewURL, e.api.cfg.PreviewToken, e.api.cfg.BaseURL = pd.URL, "preview-token-0123456789", e.srv.URL
	if r, _ := c.do("POST", start, nil, nil); r.StatusCode != 409 {
		t.Fatalf("preview of an unsigned build: %d", r.StatusCode)
	}
	e.db.Pool.Exec(t.Context(), `UPDATE builds SET state='published' WHERE id=$1`, b.ID)
	e.db.PutArtifact(t.Context(), org, store.Artifact{BuildID: b.ID, Kind: "disk.raw.zst", Digest: "sha256:" + strings.Repeat("cd", 32), Size: 1, Signed: true})
	var s struct {
		Session struct{ ID string }
		WS      string
	}
	c.must("POST", start, nil, 201, &s)
	c.must("GET", "/v1/orgs/"+org+"/previews/"+s.Session.ID, nil, 200, nil)

	// WebSocket: the session cookie is required and the Origin must be ours.
	ws := func(origin string, cookie bool) (string, *bufio.Reader, net.Conn) {
		conn, err := net.Dial("tcp", strings.TrimPrefix(e.srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		k := make([]byte, 16)
		rand.Read(k)
		req := "GET " + s.WS + " HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nOrigin: " + origin + "\r\nSec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(k) + "\r\n"
		if cookie {
			u, _ := http.NewRequest("GET", e.srv.URL, nil)
			for _, ck := range c.hc.Jar.Cookies(u.URL) {
				req += "Cookie: " + ck.Name + "=" + ck.Value + "\r\n"
			}
		}
		io.WriteString(conn, req+"\r\n")
		br := bufio.NewReader(conn)
		status, _ := br.ReadString('\n')
		for {
			l, _ := br.ReadString('\n')
			if l == "\r\n" || l == "" {
				break
			}
		}
		return status, br, conn
	}
	if st, _, conn := ws(e.srv.URL, false); !strings.Contains(st, "401") {
		t.Fatalf("anonymous websocket: %q", st)
	} else {
		conn.Close()
	}
	if st, _, conn := ws("https://evil.example", true); !strings.Contains(st, "403") {
		t.Fatalf("cross-origin websocket: %q", st)
	} else {
		conn.Close()
	}
	st, br, conn := ws(e.srv.URL, true)
	defer conn.Close()
	if !strings.Contains(st, "101") {
		t.Fatalf("websocket: %q", st)
	}
	h := make([]byte, 2)
	io.ReadFull(br, h)
	msg := make([]byte, h[1]&0x7f)
	io.ReadFull(br, msg)
	if string(msg) != "RFB 003.008\n" {
		t.Fatalf("rfb greeting %q (header % x)", msg, h)
	}
	payload, mask := []byte("hello"), []byte{9, 8, 7, 6}
	frame := append([]byte{0x82, 0x80 | byte(len(payload))}, mask...)
	for i, x := range payload {
		frame = append(frame, x^mask[i%4])
	}
	conn.Write(frame)
	io.ReadFull(br, h)
	msg = make([]byte, h[1]&0x7f)
	io.ReadFull(br, msg)
	if string(msg) != "HELLO" {
		t.Fatalf("bridged reply %q", msg)
	}
	// per-org quota (2), then stop frees a slot
	c.must("POST", start, nil, 201, nil)
	if r, _ := c.do("POST", start, nil, nil); r.StatusCode != 429 {
		t.Fatalf("third preview: %d", r.StatusCode)
	}
	c.must("DELETE", "/v1/orgs/"+org+"/previews/"+s.Session.ID, nil, 204, nil)
	c.must("POST", start, nil, 201, nil)
}

func TestBlobHeadAndRange(t *testing.T) {
	e := newEnv(t)
	c, org, _ := setupOrg(t, e, "blob@example.com", "blob-org")
	doc := []byte(`{"version":1,"panels":[{"edge":"bottom","style":"dock","size":48,"items":["launcher"]}]}`)
	code, _, digest := doUpload(t, c, org, "layout", "application/json", doc)
	if code != 201 {
		t.Fatalf("upload: %d", code)
	}
	var a struct{ URL string }
	c.must("GET", "/v1/orgs/"+org+"/assets/"+digest, nil, 200, &a)
	resp, _ := c.do("HEAD", a.URL, nil, nil)
	if resp.StatusCode != 200 || resp.ContentLength != int64(len(doc)) {
		t.Fatalf("HEAD: %d len %d", resp.StatusCode, resp.ContentLength)
	}
	resp, body := c.do("GET", a.URL, nil, map[string]string{"Range": "bytes=2-6"})
	if resp.StatusCode != 206 || string(body) != string(doc[2:7]) {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}
	// a tampered signature is refused for HEAD as well
	if r, _ := c.do("HEAD", strings.Replace(a.URL, "sig=", "sig=00", 1), nil, nil); r.StatusCode != 403 {
		t.Fatalf("bad signature: %d", r.StatusCode)
	}
}

func TestAcceptFinding(t *testing.T) {
	e := newEnv(t)
	owner, org, proj := setupOrg(t, e, "acc@example.com", "acc-org")
	base := "/v1/orgs/" + org
	var sv struct{ SpecVersion struct{ ID string } }
	owner.must("POST", base+"/projects/"+proj+"/specs", vmSpec(t), 201, &sv)
	var cr struct{ Build struct{ ID string } }
	owner.must("POST", base+"/builds", map[string]any{"specVersionId": sv.SpecVersion.ID}, 202, &cr)
	id := cr.Build.ID
	if _, err := e.db.RecordScan(t.Context(), store.Scan{BuildID: id, Scanner: "diff", ToolVersion: "t", Status: "flagged"},
		[]store.FindingRow{{Rule: "DIFF-UNOWNED", Severity: "medium", Path: "/usr/lib/x", Weight: 10}}); err != nil {
		t.Fatal(err)
	}
	owner.must("POST", base+"/members", map[string]string{"email": "rv@example.com", "role": "reviewer"}, 201, nil)
	owner.must("POST", base+"/members", map[string]string{"email": "bd@example.com", "role": "builder"}, 201, nil)
	rv, bd := e.login("rv@example.com"), e.login("bd@example.com")
	var sec struct{ Findings []struct{ ID string } }
	rv.must("GET", base+"/builds/"+id+"/security", nil, 200, &sec)
	if len(sec.Findings) != 1 {
		t.Fatalf("security view: %+v", sec)
	}
	f := sec.Findings[0].ID
	path := base + "/builds/" + id + "/findings/" + f + "/accept"
	if r, _ := bd.do("POST", path, map[string]string{"reason": "ok"}, nil); r.StatusCode != 403 {
		t.Fatalf("builder accepted a finding: %d", r.StatusCode)
	}
	if r, _ := rv.do("POST", path, map[string]string{"reason": "  "}, nil); r.StatusCode != 422 {
		t.Fatalf("accept without reason: %d", r.StatusCode)
	}
	if r, _ := rv.do("POST", base+"/builds/"+id+"/findings/fnd_nope/accept", map[string]string{"reason": "x"}, nil); r.StatusCode != 404 {
		t.Fatalf("unknown finding: %d", r.StatusCode)
	}
	rv.must("POST", path, map[string]string{"reason": "package postinst writes this file, reviewed"}, 204, nil)
	fs, _ := e.db.Findings(t.Context(), id)
	if fs[0].AcceptedBy == nil || fs[0].AcceptedReason == nil {
		t.Fatal("acceptance not recorded")
	}
	var n int
	e.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE action='finding.accept' AND org_id=$1`, org).Scan(&n)
	if n != 1 {
		t.Fatalf("audit entries: %d", n)
	}
}

// fakeIdP is a minimal OpenID provider (discovery, JWKS, token endpoint).
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	nonce string
	email string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{email: "sso.user@example.com"}
	f.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	b64 := base64.RawURLEncoding.EncodeToString
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
		c, _ := json.Marshal(map[string]any{"iss": f.srv.URL, "aud": "client", "sub": "sso-1", "email": f.email, "email_verified": true, "name": "SSO User", "nonce": f.nonce, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
		in := b64(h) + "." + b64(c)
		d := sha256.Sum256([]byte(in))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, d[:])
		json.NewEncoder(w).Encode(map[string]string{"id_token": in + "." + b64(sig)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestOIDCLogin(t *testing.T) {
	e := newEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &client{e: e, hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	// not configured
	for _, p := range []string{"/v1/auth/oidc/start", "/v1/auth/oidc/callback?state=x&code=y"} {
		if r, _ := c.do("GET", p, nil, nil); r.StatusCode != 404 {
			t.Fatalf("%s unconfigured: %d", p, r.StatusCode)
		}
	}
	idp := newFakeIdP(t)
	e.api.cfg.OIDC = &oidc.Provider{Issuer: idp.srv.URL, ClientID: "client", RedirectURL: e.srv.URL + "/v1/auth/oidc/callback"}
	resp, _ := c.do("GET", "/v1/auth/oidc/start?redirect=//evil.example", nil, nil)
	if resp.StatusCode != 302 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("state") == "" {
		t.Fatalf("authorization request: %s", loc)
	}
	idp.nonce = loc.Query().Get("nonce")
	state := loc.Query().Get("state")
	cb := func(state, code string) *http.Response {
		r, _ := c.do("GET", "/v1/auth/oidc/callback?state="+state+"&code="+code, nil, nil)
		return r
	}
	if r := cb("unknown-state", "good-code"); r.StatusCode != 400 {
		t.Fatalf("unknown state: %d", r.StatusCode)
	}
	if r := cb(state, "good-code"); r.StatusCode != 302 || r.Header.Get("Location") != "/" { // open redirect refused
		t.Fatalf("callback: %d → %q", r.StatusCode, r.Header.Get("Location"))
	}
	var me struct{ User struct{ Email string } }
	c.must("GET", "/v1/me", nil, 200, &me)
	if me.User.Email != idp.email {
		t.Fatalf("signed in as %q", me.User.Email)
	}
	if r := cb(state, "good-code"); r.StatusCode != 400 { // state is single-use
		t.Fatalf("state replay: %d", r.StatusCode)
	}
	// a rejected code does not sign anyone in
	c2 := &client{e: e, hc: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	r, _ := c2.do("GET", "/v1/auth/oidc/start", nil, nil)
	loc, _ = url.Parse(r.Header.Get("Location"))
	if r, _ := c2.do("GET", "/v1/auth/oidc/callback?state="+loc.Query().Get("state")+"&code=bad", nil, nil); r.StatusCode != 401 {
		t.Fatalf("bad code: %d", r.StatusCode)
	}
}
