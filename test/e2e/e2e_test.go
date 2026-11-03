//go:build e2e

// End-to-end test against a running stack (scripts/dev-up.sh):
//
//	FOUNDRY_E2E_URL=http://127.0.0.1:8080 go test -tags e2e -timeout 3h ./test/e2e/
//
// spec → build (sandbox, egress proxy) → scan → boot test → auto-approve →
// finalize (signer) → publish → download with digest + signature checks.
package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foundry.dev/foundry/internal/client"
)

func TestPipeline(t *testing.T) {
	base := os.Getenv("FOUNDRY_E2E_URL")
	if base == "" {
		t.Skip("FOUNDRY_E2E_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	c := client.New(base)
	suffix := fmt.Sprint(time.Now().Unix() % 100000)
	if err := c.EmailLogin(ctx, "e2e-"+suffix+"@example.com", nil); err != nil {
		t.Fatalf("login (needs FOUNDRY_DEV=1 on the API): %v", err)
	}
	org, err := c.CreateOrg(ctx, "E2E "+suffix, "e2e-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	c.Org = org
	tok, err := c.CreateToken(ctx, "e2e", []string{"specs:write", "builds:write"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	c.Token, c.CSRF = tok, ""
	p, err := c.CreateProject(ctx, "CI server", "ci-server")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := os.ReadFile("../golden/specs/vm-server.yaml")
	sv, _, err := c.PushSpec(ctx, p.ID, spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.CreateBuild(ctx, sv.ID, []string{"qcow2"}, "e2e-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("build %s queued", b.ID)
	var lines int
	go c.FollowLogs(ctx, b.ID, 0, func(l client.LogLine) {
		lines++
		if strings.HasPrefix(l.Line, "foundry:") || strings.HasPrefix(l.Line, "tester:") || strings.HasPrefix(l.Line, "finalize:") {
			t.Logf("[%s] %s", l.Stream, l.Line)
		}
	}, func(s string) { t.Logf("state → %s", s) })
	b, err = c.WaitState(ctx, b.ID, "awaiting_review")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != "published" {
		t.Fatalf("build ended in %s: %s", b.State, b.Failure)
	}
	if b.Tier == nil || *b.Tier != "verified" && *b.Tier != "private-unverified" {
		t.Fatalf("tier %v", b.Tier)
	}
	as, err := c.Artifacts(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	got := map[string]string{}
	for _, a := range as {
		if a.URL == "" {
			continue
		}
		switch a.Kind {
		case "qcow2", "sha256sums", "sha256sums.sig", "pubkey-artifact", "sbom", "uki":
			p := filepath.Join(dir, a.Name)
			if err := c.Download(ctx, a, p); err != nil {
				t.Fatalf("%s: %v", a.Kind, err)
			}
			got[a.Kind] = p
		}
	}
	for _, k := range []string{"qcow2", "sha256sums", "sha256sums.sig", "pubkey-artifact", "sbom", "uki"} {
		if got[k] == "" {
			t.Fatalf("artifact %s not downloadable; have %v", k, got)
		}
	}
	// SHA256SUMS is signed by the tenant's artifact key and lists the UKI.
	sums, _ := os.ReadFile(got["sha256sums"])
	sig, _ := os.ReadFile(got["sha256sums.sig"])
	pubPEM, _ := os.ReadFile(got["pubkey-artifact"])
	blk, _ := pem.Decode(pubPEM)
	pk, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil || !ed25519.Verify(pk.(ed25519.PublicKey), sums, sig) {
		t.Fatal("SHA256SUMS signature does not verify")
	}
	// Every installable download is listed in the signed SHA256SUMS.
	for _, k := range []string{"uki", "qcow2"} {
		f, err := os.Open(got[k])
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		io.Copy(h, f)
		f.Close()
		line := hex.EncodeToString(h.Sum(nil)) + "  " + filepath.Base(got[k])
		if !strings.Contains(string(sums), line) {
			t.Fatalf("%s (%s) is not listed in SHA256SUMS:\n%s", k, line, sums)
		}
	}
	if lines < 50 {
		t.Fatalf("only %d log lines streamed", lines)
	}
	t.Logf("published %s; %d artifacts verified", b.ID, len(got))
}
