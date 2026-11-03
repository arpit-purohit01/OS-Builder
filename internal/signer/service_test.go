package signer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/testdb"
)

// The signer signs only for builds in "finalizing" (approved), only the
// build's own recorded inputs, and only with the right bearer token.
func TestServiceAuthorization(t *testing.T) {
	need(t, "openssl")
	db := testdb.New(t)
	ctx := context.Background()
	sd := testdb.NewSeed(t, db, "acme")
	b := sd.Build(t, 0, 5)
	rh := strings.Repeat("ab", 32)
	if err := db.SetBuildResult(ctx, b.ID, rh, ""); err != nil {
		t.Fatal(err)
	}
	bs, _ := blob.NewFS(t.TempDir())
	svc := &Service{DB: db, Blobs: bs, KeysRoot: t.TempDir(), Tools: DefaultTools(), Token: "secret-token-0123456789",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	call := func(token, op string, body map[string]string) (int, map[string]string) {
		j, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/builds/"+b.ID+"/"+op, bytes.NewReader(j))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := call("wrong", "verity", map[string]string{"roothash": rh}); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	// queued build: refused
	if code, out := call(svc.Token, "verity", map[string]string{"roothash": rh}); code != 403 || !strings.Contains(out["error"], "finalizing") {
		t.Fatalf("unapproved build signed: %d %v", code, out)
	}
	// walk the build to finalizing through the state machine
	for _, st := range [][2]string{{"queued", "leased"}, {"leased", "building"}, {"building", "built"}, {"built", "scanning"},
		{"scanning", "scanned"}, {"scanned", "testing"}, {"testing", "tested"}, {"tested", "approved"}, {"approved", "finalizing"}} {
		if err := db.Transition(ctx, b.ID, st[0], st[1], store.TransitionOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := call(svc.Token, "verity", map[string]string{"roothash": strings.Repeat("cd", 32)}); code != 403 {
		t.Fatalf("foreign root hash signed: %d", code)
	}
	code, out := call(svc.Token, "verity", map[string]string{"roothash": rh})
	if code != 200 || out["digest"] == "" {
		t.Fatalf("verity: %d %v", code, out)
	}
	// the UKI must be the build's recorded artifact
	if code, _ := call(svc.Token, "uki", map[string]string{"digest": "sha256:" + strings.Repeat("0", 64)}); code != 403 {
		t.Fatalf("unrecorded UKI accepted: %d", code)
	}
	// blobs: only signable artifact kinds of this build
	d, n, _ := bs.Put(ctx, strings.NewReader("abc  file\n"), "", 0)
	db.PutArtifact(ctx, sd.Org.ID, store.Artifact{BuildID: b.ID, Kind: "boot-log", Digest: d, Size: n})
	if code, _ := call(svc.Token, "blob", map[string]string{"digest": d}); code != 403 {
		t.Fatalf("non-signable kind signed: %d", code)
	}
	db.PutArtifact(ctx, sd.Org.ID, store.Artifact{BuildID: b.ID, Kind: "sha256sums", Digest: d, Size: n})
	if code, _ := call(svc.Token, "blob", map[string]string{"digest": d}); code != 403 {
		t.Fatalf("SHA256SUMS without the build's usr partition signed: %d", code)
	}
	d, n, _ = bs.Put(ctx, strings.NewReader(strings.Repeat("0", 64)+"  ci_abababab-abab-abab-abab-abababababab.usr.raw\n"), "", 0)
	db.PutArtifact(ctx, sd.Org.ID, store.Artifact{BuildID: b.ID, Kind: "sha256sums", Digest: d, Size: n})
	if code, out := call(svc.Token, "blob", map[string]string{"digest": d}); code != 200 || out["digest"] == "" {
		t.Fatalf("sha256sums: %d %v", code, out)
	}
	// every signature is audited and the chain verifies
	if bad, err := db.VerifyAuditChain(ctx); err != nil || bad != 0 {
		t.Fatalf("audit chain: %d %v", bad, err)
	}
	var cnt int
	db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE actor='signer'`).Scan(&cnt)
	if cnt != 2 {
		t.Fatalf("audited signatures: %d", cnt)
	}
}
