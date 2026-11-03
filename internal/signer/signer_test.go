package signer

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func need(t *testing.T, tools ...string) {
	for _, x := range tools {
		if _, err := exec.LookPath(x); err != nil {
			t.Skipf("%s not installed", x)
		}
	}
}

func TestKeysAndBlobs(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(root, "Bad Tenant", true); err == nil {
		t.Fatal("invalid tenant accepted")
	}
	if _, err := Open(root, "acme", false); err == nil {
		t.Fatal("missing keys must error without create")
	}
	k, err := Open(root, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(k.KeyPath(RoleDB))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	k2, err := Open(root, "acme", false) // reopen, no regeneration
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := k.Cert(RoleDB)
	c2, _ := k2.Cert(RoleDB)
	if !bytes.Equal(c1.Raw, c2.Raw) {
		t.Fatal("keys regenerated on reopen")
	}
	kek, _ := k.Cert(RoleKEK)
	if err := c1.CheckSignatureFrom(kek); err != nil {
		t.Fatalf("db not signed by KEK: %v", err)
	}
	sig, err := k.SignBlob([]byte("SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := os.ReadFile(filepath.Join(k.Dir, "artifact.pub.pem"))
	if !VerifyBlob(pub, []byte("SHA256SUMS"), sig) || VerifyBlob(pub, []byte("tampered"), sig) {
		t.Fatal("blob signature verification")
	}
	if g := k.OwnerGUID(); len(g) != 36 || g != k2.OwnerGUID() {
		t.Fatalf("guid %s", g)
	}
}

func TestVeritySig(t *testing.T) {
	need(t, "openssl")
	k, _ := Open(t.TempDir(), "acme", true)
	ctx, tools := context.Background(), DefaultTools()
	rh := strings.Repeat("ab", 32)
	p, err := k.VeritySig(ctx, tools, rh)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != VeritySigSize {
		t.Fatalf("size %d", len(p))
	}
	got, err := k.VerifyVeritySig(ctx, tools, p)
	if err != nil || got != rh {
		t.Fatalf("verify: %v %s", err, got)
	}
	// A different key must not verify.
	other, _ := Open(t.TempDir(), "other", true)
	if _, err := other.VerifyVeritySig(ctx, tools, p); err == nil {
		t.Fatal("foreign signature verified")
	}
	if _, err := k.VeritySig(ctx, tools, "nothex"); err == nil {
		t.Fatal("bad root hash accepted")
	}
}

func TestBootloaderUKIAndEnrollment(t *testing.T) {
	need(t, "sbsign", "sbverify", "ukify", "cert-to-efi-sig-list", "sign-efi-sig-list")
	tools := DefaultTools()
	if _, err := os.Stat(tools.Stubs["ubuntu-24.04"]); err != nil {
		t.Skip("systemd-boot-efi not installed")
	}
	if _, err := os.Stat("/usr/lib/systemd/systemd-measure"); err != nil {
		t.Skip("systemd-measure not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	k, _ := Open(filepath.Join(dir, "keys"), "acme", true)
	// bootloader
	bl := filepath.Join(dir, "BOOTX64.EFI")
	if err := k.SignBootloader(ctx, tools, "ubuntu-24.04", bl); err != nil {
		t.Fatal(err)
	}
	if err := k.SignBootloader(ctx, tools, "plan9", bl); err == nil {
		t.Fatal("unknown base accepted")
	}
	// An unsigned UKI like mkosi produces (fake kernel/initrd are enough for the PE plumbing).
	linux := filepath.Join(dir, "vmlinuz")
	os.WriteFile(linux, bytes.Repeat([]byte("K"), 4096), 0o644)
	initrd := filepath.Join(dir, "initrd")
	os.WriteFile(initrd, bytes.Repeat([]byte("I"), 4096), 0o644)
	unsigned := filepath.Join(dir, "unsigned.efi")
	rh := strings.Repeat("cd", 32)
	cmd := exec.Command("ukify", "build", "--linux="+linux, "--initrd="+initrd, "--cmdline=quiet usrhash="+rh+" mount.usrflags=ro",
		"--uname=6.8.0-test", "--os-release=ID=foundry\n", "--output="+unsigned)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ukify: %v %s", err, out)
	}
	if err := k.VerifyPE(ctx, tools, unsigned); err == nil {
		t.Fatal("unsigned UKI verified")
	}
	signed := filepath.Join(dir, "signed.efi")
	info, err := k.BuildUKI(ctx, tools, "ubuntu-24.04", unsigned, signed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Usrhash != rh || info.Uname != "6.8.0-test" {
		t.Fatalf("info %+v", info)
	}
	sec, _ := UKISections(signed)
	if !bytes.Equal(sec[".linux"][:4096], bytes.Repeat([]byte("K"), 4096)) || !strings.Contains(string(sec[".pcrsig"]), "sha256") {
		t.Fatalf("sections: pcrsig=%q", sec[".pcrsig"])
	}
	// A UKI without usrhash is refused.
	bad := filepath.Join(dir, "bad.efi")
	exec.Command("ukify", "build", "--linux="+linux, "--cmdline=quiet", "--uname=6.8.0-test", "--output="+bad).Run()
	if _, err := k.BuildUKI(ctx, tools, "ubuntu-24.04", bad, filepath.Join(dir, "x.efi")); err == nil {
		t.Fatal("UKI without usrhash accepted")
	}
	// enrollment, with an extra (third-party) db certificate in DER form
	other, _ := Open(filepath.Join(dir, "keys"), "thirdparty", true)
	oc, _ := other.Cert(RoleDB)
	der := filepath.Join(dir, "third.der")
	os.WriteFile(der, oc.Raw, 0o644)
	plain, _ := k.EnrollmentFiles(ctx, tools)
	tools.ExtraDBCerts = []string{der}
	files, err := k.EnrollmentFiles(ctx, tools)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"PK.auth", "KEK.auth", "db.auth"} {
		if len(files[n]) < 500 {
			t.Fatalf("%s too small", n)
		}
	}
	if len(files["db.auth"]) <= len(plain["db.auth"])+len(oc.Raw) || !bytes.Contains(files["db.auth"], oc.Raw) {
		t.Fatal("extra db certificate not included")
	}
}

func TestCheckSums(t *testing.T) {
	rh := strings.Repeat("ab", 32)
	usr := "x_abababab-abab-abab-abab-abababababab.usr.raw"
	ok := strings.Repeat("0", 64) + "  " + usr + "\n" + strings.Repeat("1", 64) + "  x.efi\n"
	if err := checkSums([]byte(ok), rh); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"other build": strings.Repeat("0", 64) + "  x_cdcdcdcd-abab-abab-abab-abababababab.usr.raw\n",
		"malformed":   strings.Repeat("0", 64) + " " + usr + "\n",
		"injection":   ok + "-----BEGIN PGP SIGNED MESSAGE-----\n",
	} {
		if checkSums([]byte(bad), rh) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInstallerOSRelease(t *testing.T) {
	got := installerOSRelease("ID=ubuntu\nPRETTY_NAME=\"Aurora OS 1.0\"\nIMAGE_ID=aurora")
	if !strings.Contains(got, `PRETTY_NAME="Aurora OS 1.0 Installer"`) || !strings.Contains(got, "IMAGE_ID=aurora") {
		t.Fatal(got)
	}
}
