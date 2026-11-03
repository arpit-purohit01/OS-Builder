package osspec

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	d, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		d = filepath.Dir(d)
	}
	t.Fatal("repo root not found")
	return ""
}

func TestSchemaInSyncWithDocs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs/schemas/osspec.v1alpha1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(doc), bytes.TrimSpace(schemaJSON)) {
		t.Fatal("internal/osspec/schema.v1alpha1.json differs from docs/schemas copy; copy it over")
	}
}

func TestExamplesLoad(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(repoRoot(t), "docs/examples/*.yaml"))
	if len(files) < 3 {
		t.Fatalf("expected examples, got %v", files)
	}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		l, err := Load(data)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !strings.HasPrefix(l.Digest, "sha256:") || len(l.Digest) != 71 {
			t.Fatalf("bad digest %s", l.Digest)
		}
	}
}

const minimal = `apiVersion: foundry.dev/v1alpha1
kind: OSSpec
metadata: {name: demo-os, version: 1.0.0}
foundation: {base: ubuntu-24.04, profile: desktop-gnome}
`

func TestDigestStableAcrossFormats(t *testing.T) {
	a, err := Load([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	js := `{"foundation":{"profile":"desktop-gnome","base":"ubuntu-24.04"},"metadata":{"version":"1.0.0","name":"demo-os"},"kind":"OSSpec","apiVersion":"foundry.dev/v1alpha1"}`
	b, err := Load([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("digests differ: %s %s", a.Digest, b.Digest)
	}
	// Explicitly writing a default must not change the digest.
	c, err := Load([]byte(minimal + "security: {level: balanced}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest != a.Digest {
		t.Fatal("explicit default changed digest")
	}
	// Canonical JSON must itself re-load to the same digest (idempotent).
	d, err := Load(a.Canonical)
	if err != nil || d.Digest != a.Digest {
		t.Fatalf("canonical reload mismatch: %v", err)
	}
}

func codes(err error) []string {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out []string
	for _, e := range ve.Errors {
		out = append(out, e.Code)
	}
	return out
}

func TestRejections(t *testing.T) {
	cases := map[string]struct {
		extra string
		code  string
	}{
		"sb off needs relaxed":    {"security: {secureBoot: \"off\"}\n", SemSecureBootOffNeedsRelaxed},
		"developer hardened":      {"security: {level: hardened}\naccess: {rootModel: developer}\n", SemDeveloperNotHardened},
		"bad cmdline":             {"kernel: {extraCmdline: [\"systemd.unit=emergency.target\"]}\n", SemCmdlineDenied},
		"trademark":               {"branding: {osName: \"Ubuntu Pro Max\"}\n", SemTrademark},
		"oidc missing":            {"ecosystem: {identity: {provider: oidc}}\n", SemIdentityFields},
		"kiosk section":           {"ecosystem: {kiosk: {url: \"https://x.example.com\"}}\n", SemKioskSectionProfile},
		"unknown field":           {"surprise: 1\n", "SCH-001"},
		"bad accent":              {"look: {theme: {accent: red}}\n", "SCH-001"},
		"float version":           {"kernel: {flavor: 1.5}\n", "SCH-001"},
		"locked with admin user":  {"access: {rootModel: locked, users: {mode: predefined, predefined: [{name: bob, admin: true}]}}\n", SemPredefinedUsers},
		"http issuer not allowed": {"ecosystem: {identity: {provider: oidc, issuer: \"http://idp.example.com\", clientId: x}}\n", "SCH-001"},
	}
	for name, c := range cases {
		_, err := Load([]byte(minimal + c.extra))
		got := codes(err)
		found := false
		for _, g := range got {
			if g == c.code {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %s, got %v (%v)", name, c.code, got, err)
		}
	}
}

func TestKioskRules(t *testing.T) {
	base := `apiVersion: foundry.dev/v1alpha1
kind: OSSpec
metadata: {name: lobby, version: 1.0.0}
foundation: {base: ubuntu-24.04, profile: kiosk-cage}
`
	if _, err := Load([]byte(base)); !contains(codes(err), SemKioskNeedsURL) {
		t.Fatalf("expected kiosk url error, got %v", err)
	}
	l, err := Load([]byte(base + "ecosystem: {kiosk: {url: \"https://k.example.com\"}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Spec.Access.RootModel != "locked" || l.Spec.Access.Users.Mode != "autologin-kiosk" {
		t.Fatalf("kiosk defaults wrong: %+v", l.Spec.Access)
	}
	if _, err := Load([]byte(base + "ecosystem: {kiosk: {url: \"https://k.example.com\"}}\nsecurity: {diskEncryption: tpm2+pin}\n")); !contains(codes(err), SemTPMPinNotKiosk) {
		t.Fatalf("expected pin error: %v", err)
	}
}

func TestMigrationFromV1alpha0(t *testing.T) {
	old := `apiVersion: foundry.dev/v1alpha0
kind: OSSpec
metadata: {name: legacy-os, version: 0.9.0}
foundation: {distro: ubuntu, profile: desktop-kde}
rootAccess: full
`
	l, err := Load([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if !l.Migrated || l.Spec.Foundation.Base != "ubuntu-24.04" || l.Spec.Access.RootModel != "developer" {
		t.Fatalf("migration wrong: %+v %+v", l.Spec.Foundation, l.Spec.Access)
	}
	if len(l.Warnings) < 2 {
		t.Fatalf("expected migration warnings: %v", l.Warnings)
	}
	if _, err := Load([]byte(strings.Replace(old, "v1alpha0", "v0", 1))); !contains(codes(err), "MIG-404") {
		t.Fatalf("expected unsupported version: %v", err)
	}
}

func TestParseLimitsAndYAMLTimestamps(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxSpecBytes+1)
	if _, err := Load(big); !contains(codes(err), "PARSE-001") {
		t.Fatal("expected size error")
	}
	l, err := Load([]byte(minimal[:len(minimal)-2] + ", snapshot: 2026-10-20T00:00:00Z}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Spec.Foundation.Snapshot != "2026-10-20T00:00:00Z" {
		t.Fatalf("snapshot %q", l.Spec.Foundation.Snapshot)
	}
}

func TestCmdlineAllowed(t *testing.T) {
	for p, want := range map[string]bool{
		"quiet": true, "loglevel=3": true, "i915.enable_psr=0": true, "init=/bin/sh": false,
		"rd.break": false, "lockdown=none": false, "rw": false, "random=1": false, "quietx": false,
	} {
		if got := CmdlineAllowed(p); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
