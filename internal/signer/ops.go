package signer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"foundry.dev/foundry/internal/uki"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Tools are the external programs the signer drives. Paths default to PATH lookups.
type Tools struct {
	Sbsign, Sbverify, Ukify, Openssl, CertToESL, SignESL string
	// Stub is the platform's trusted systemd-stub for each base. The stub from
	// a (customer-influenced) build is never used for signed UKIs.
	Stubs map[string]string
	// Bootloaders are the platform's trusted systemd-boot binaries per base.
	Bootloaders map[string]string
	// ExtraDBCerts are added to db next to the tenant certificate (PEM or DER),
	// e.g. the Microsoft UEFI CA 2011/2023 so GPU/NIC option ROMs and
	// Windows keep working on real hardware (ADR-0006).
	ExtraDBCerts []string
}

// MicrosoftOwnerGUID is the signature owner used for Microsoft certificates.
const MicrosoftOwnerGUID = "77fa9abd-0359-4d32-bd60-28f4e78f784b"

// DefaultTools uses the host's tools and the distro's systemd-boot/stub.
func DefaultTools() Tools {
	t := Tools{Sbsign: "sbsign", Sbverify: "sbverify", Ukify: "ukify", Openssl: "openssl",
		CertToESL: "cert-to-efi-sig-list", SignESL: "sign-efi-sig-list",
		Stubs: map[string]string{}, Bootloaders: map[string]string{}}
	for _, b := range []string{"ubuntu-24.04", "ubuntu-26.04", "debian-13"} {
		t.Stubs[b] = "/usr/lib/systemd/boot/efi/linuxx64.efi.stub"
		t.Bootloaders[b] = "/usr/lib/systemd/boot/efi/systemd-bootx64.efi"
	}
	return t
}

func run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(tail(out.String(), 2000)))
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// SignPE Authenticode-signs a PE binary (bootloader, UKI) with the db key.
func (k *Keys) SignPE(ctx context.Context, t Tools, in, out string) error {
	if err := run(ctx, t.Sbsign, "--key", k.KeyPath(RoleDB), "--cert", k.CertPath(RoleDB), "--output", out, in); err != nil {
		return err
	}
	return run(ctx, t.Sbverify, "--cert", k.CertPath(RoleDB), out)
}

// VerifyPE checks a PE signature against the tenant db certificate.
func (k *Keys) VerifyPE(ctx context.Context, t Tools, path string) error {
	return run(ctx, t.Sbverify, "--cert", k.CertPath(RoleDB), path)
}

// UKISections and InspectUKI are re-exported from internal/uki.
func UKISections(path string) (map[string][]byte, error) { return uki.Sections(path) }

// UKIInfo describes a UKI's identity-relevant content.
type UKIInfo = uki.Info

// InspectUKI parses .cmdline/.uname/.osrel.
func InspectUKI(path string) (*UKIInfo, error) { return uki.Inspect(path) }

// BuildUKI re-assembles a UKI from an unsigned build output using the
// platform's trusted stub, embeds a signed TPM2 PCR 11 policy (.pcrsig,
// .pcrpkey) and Secure Boot-signs it. Only .linux, .initrd, .cmdline,
// .osrel, .uname and .splash are taken from the build.
// InstallerCmdline is appended to the command line of the installer UKI
// variant: a live system with a RAM root (the boot medium is never written),
// first-boot partitioning disabled, and the installer started on tty1. It is
// a fixed string; no build or user input reaches a signed command line.
const InstallerCmdline = "foundry.installer=1 root=tmpfs rd.systemd.mask=systemd-repart.service systemd.mask=systemd-repart.service systemd.unit=multi-user.target"

// UKIVariant selects which signed UKI BuildUKI produces.
type UKIVariant string

const (
	VariantDefault   UKIVariant = ""
	VariantInstaller UKIVariant = "installer"
)

func (k *Keys) BuildUKI(ctx context.Context, t Tools, base, unsigned, out string, variant ...UKIVariant) (*UKIInfo, error) {
	stub := t.Stubs[base]
	if stub == "" {
		return nil, fmt.Errorf("signer: no trusted stub for base %s", base)
	}
	sec, err := UKISections(unsigned)
	if err != nil {
		return nil, err
	}
	info, err := InspectUKI(unsigned)
	if err != nil {
		return nil, err
	}
	if info.Usrhash == "" {
		return nil, errors.New("signer: UKI command line has no usrhash= (unverified /usr)")
	}
	cmdline, osrel := info.Cmdline, info.OSRel
	if len(variant) > 0 && variant[0] == VariantInstaller {
		cmdline += " " + InstallerCmdline
		osrel = installerOSRelease(osrel)
	} else if len(variant) > 0 && variant[0] != VariantDefault {
		return nil, fmt.Errorf("signer: unknown UKI variant %q", variant[0])
	}
	dir, err := os.MkdirTemp("", "foundry-uki-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0o600)
		return p
	}
	args := []string{"build",
		"--linux=" + write("linux", sec[".linux"]),
		"--initrd=" + write("initrd", sec[".initrd"]),
		"--cmdline=@" + write("cmdline", []byte(cmdline)),
		"--os-release=@" + write("osrel", []byte(osrel+"\n")),
		"--stub=" + stub,
		"--secureboot-private-key=" + k.KeyPath(RoleDB),
		"--secureboot-certificate=" + k.CertPath(RoleDB),
		"--signtool=sbsign",
		"--no-sign-kernel", // the stub verifies the whole UKI; the embedded kernel needs no own signature
		"--pcr-private-key=" + k.KeyPath(RolePCR),
		"--pcr-public-key=" + k.CertPath(RolePCR),
		"--pcr-banks=sha256",
		// Sign the PCR 11 policy for the initrd phase only: the root disk can be
		// unlocked in the initrd, never from the running system (Phase 0, P0-06).
		"--phases=enter-initrd",
		"--output=" + out,
	}
	if info.Uname != "" {
		args = append(args, "--uname="+info.Uname)
	}
	if sp := sec[".splash"]; len(sp) > 0 {
		args = append(args, "--splash="+write("splash.bmp", sp))
	}
	if err := run(ctx, t.Ukify, args...); err != nil {
		return nil, err
	}
	if err := k.VerifyPE(ctx, t, out); err != nil {
		return nil, err
	}
	got, err := UKISections(out)
	if err != nil {
		return nil, err
	}
	if len(got[".pcrsig"]) == 0 || len(got[".pcrpkey"]) == 0 {
		return nil, errors.New("signer: ukify did not embed the PCR signature")
	}
	return info, nil
}

// installerOSRelease marks the installer entry in the boot menu.
func installerOSRelease(osrel string) string {
	lines := strings.Split(osrel, "\n")
	found := false
	for i, l := range lines {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			lines[i] = "PRETTY_NAME=" + strconv.Quote(strings.Trim(v, `"'`)+" Installer")
			found = true
		}
	}
	if !found {
		lines = append(lines, `PRETTY_NAME="Installer"`)
	}
	return strings.Join(lines, "\n")
}

// SignBootloader signs the platform's trusted systemd-boot for a base.
func (k *Keys) SignBootloader(ctx context.Context, t Tools, base, out string) error {
	src := t.Bootloaders[base]
	if src == "" {
		return fmt.Errorf("signer: no trusted bootloader for base %s", base)
	}
	return k.SignPE(ctx, t, src, out)
}

// VeritySigSize is the fixed size of a usr-verity-sig partition.
const VeritySigSize = 16 << 10

// VeritySig returns the usr-verity-sig partition payload: a JSON object with
// the root hash, the signing certificate's fingerprint and a detached PKCS#7
// signature over the hex root hash, NUL-padded to 16 KiB.
func (k *Keys) VeritySig(ctx context.Context, t Tools, roothash string) ([]byte, error) {
	if len(roothash) != 64 {
		return nil, errors.New("signer: root hash must be 64 hex characters")
	}
	if _, err := hex.DecodeString(roothash); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "foundry-verity-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, sig := filepath.Join(dir, "rh"), filepath.Join(dir, "sig")
	os.WriteFile(in, []byte(roothash), 0o600)
	if err := run(ctx, t.Openssl, "smime", "-sign", "-in", in, "-inkey", k.KeyPath(RoleDB), "-signer", k.CertPath(RoleDB),
		"-noattr", "-nocerts", "-binary", "-outform", "der", "-out", sig); err != nil {
		return nil, err
	}
	der, err := os.ReadFile(sig)
	if err != nil {
		return nil, err
	}
	cert, err := k.Cert(RoleDB)
	if err != nil {
		return nil, err
	}
	fp := sha256.Sum256(cert.Raw)
	js, _ := json.Marshal(map[string]string{"rootHash": roothash, "certificateFingerprint": hex.EncodeToString(fp[:]),
		"signature": base64.StdEncoding.EncodeToString(der)})
	if len(js) >= VeritySigSize {
		return nil, errors.New("signer: verity signature too large")
	}
	buf := make([]byte, VeritySigSize)
	copy(buf, js)
	return buf, nil
}

// VerifyVeritySig checks a verity-sig payload with openssl against the db cert.
func (k *Keys) VerifyVeritySig(ctx context.Context, t Tools, payload []byte) (string, error) {
	var v struct {
		RootHash  string `json:"rootHash"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(bytes.TrimRight(payload, "\x00"), &v); err != nil {
		return "", err
	}
	der, err := base64.StdEncoding.DecodeString(v.Signature)
	if err != nil {
		return "", err
	}
	dir, _ := os.MkdirTemp("", "foundry-vv-")
	defer os.RemoveAll(dir)
	os.WriteFile(filepath.Join(dir, "rh"), []byte(v.RootHash), 0o600)
	os.WriteFile(filepath.Join(dir, "sig"), der, 0o600)
	err = run(ctx, t.Openssl, "smime", "-verify", "-binary", "-inform", "der", "-in", filepath.Join(dir, "sig"),
		"-content", filepath.Join(dir, "rh"), "-certfile", k.CertPath(RoleDB), "-nointern", "-noverify", "-out", "/dev/null")
	return v.RootHash, err
}

// EnrollmentFiles returns signed EFI variable updates (PK.auth, KEK.auth,
// db.auth) and the public certificates for systemd-boot's
// loader/keys/<name>/ auto-enrollment.
func (k *Keys) EnrollmentFiles(ctx context.Context, t Tools) (map[string][]byte, error) {
	dir, err := os.MkdirTemp("", "foundry-enroll-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	guid := k.OwnerGUID()
	ts := "2026-01-01 00:00:00" // fixed timestamp: enrollment files are reproducible per key set
	steps := []struct{ v, role, signer string }{{"PK", RolePK, RolePK}, {"KEK", RoleKEK, RolePK}, {"db", RoleDB, RoleKEK}}
	out := map[string][]byte{}
	for _, s := range steps {
		esl := filepath.Join(dir, s.v+".esl")
		auth := filepath.Join(dir, s.v+".auth")
		if err := run(ctx, t.CertToESL, "-g", guid, k.CertPath(s.role), esl); err != nil {
			return nil, err
		}
		if s.v == "db" && len(t.ExtraDBCerts) > 0 {
			if err := appendExtraDB(ctx, t, dir, esl); err != nil {
				return nil, err
			}
		}
		if err := run(ctx, t.SignESL, "-t", ts, "-g", guid, "-k", k.KeyPath(s.signer), "-c", k.CertPath(s.signer), s.v, esl, auth); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(auth)
		if err != nil {
			return nil, err
		}
		out[s.v+".auth"] = b
		c, _ := os.ReadFile(k.CertPath(s.role))
		out[s.v+".crt"] = c
	}
	return out, nil
}

// appendExtraDB converts extra certificates to signature lists and appends
// them to the db list before it is signed.
func appendExtraDB(ctx context.Context, t Tools, dir, esl string) error {
	all, err := os.ReadFile(esl)
	if err != nil {
		return err
	}
	for i, c := range t.ExtraDBCerts {
		pemPath := filepath.Join(dir, fmt.Sprintf("extra-%d.pem", i))
		b, err := os.ReadFile(c)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("-----BEGIN CERTIFICATE-----")) {
			err = os.WriteFile(pemPath, b, 0o600)
		} else {
			err = run(ctx, t.Openssl, "x509", "-inform", "der", "-in", c, "-out", pemPath)
		}
		if err != nil {
			return err
		}
		x := filepath.Join(dir, fmt.Sprintf("extra-%d.esl", i))
		if err := run(ctx, t.CertToESL, "-g", MicrosoftOwnerGUID, pemPath, x); err != nil {
			return err
		}
		e, err := os.ReadFile(x)
		if err != nil {
			return err
		}
		all = append(all, e...)
	}
	return os.WriteFile(esl, all, 0o600)
}
