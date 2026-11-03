// Package finalize is the trusted post-approval stage (F1 in
// docs/context/build-pipeline.md §3). It takes the unsigned disk produced by
// the untrusted build and, using only signatures returned by the signer:
//
//   - replaces the UKI with one re-assembled from the platform's trusted stub,
//     carrying a signed TPM2 PCR 11 policy and an Authenticode signature;
//   - replaces systemd-boot with the platform's signed copy;
//   - installs Secure Boot enrollment files (loader/keys/foundry/*.auth) and
//     loader.conf (auto-enroll on VMs, manual on hardware);
//   - adds the deferred usr-verity-sig GPT entry and writes the signed root
//     hash into the 16 KiB gap mkosi reserved for it;
//   - re-verifies the usr partition against the root hash (veritysetup) and
//     emits the update payload (usr, usr-verity, usr-verity-sig, UKI) plus a
//     signed SHA256SUMS.
//
// No byte of the usr or usr-verity partitions changes, so the usr root hash
// of the published image equals the one that was scanned and tested.
package finalize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/gpt"
	"foundry.dev/foundry/internal/imgutil"
)

// ErrIntegrity marks inputs that do not match what was built and approved.
var ErrIntegrity = errors.New("finalize: integrity check failed")

// Signer is the signing service as seen by finalize (see internal/signclient).
// Every call is authorized by the signer against the build's state.
type Signer interface {
	SignUKI(ctx context.Context, buildID, unsignedDigest string) (signedDigest string, err error)
	InstallerUKI(ctx context.Context, buildID, unsignedDigest string) (signedDigest string, err error)
	Bootloader(ctx context.Context, buildID string) (digest string, err error)
	VeritySig(ctx context.Context, buildID, roothash string) (digest string, err error)
	Enrollment(ctx context.Context, buildID string) (files map[string]string, err error) // name → digest
	SignBlob(ctx context.Context, buildID, digest string) (sigDigest string, err error)
	PublicKeys(ctx context.Context, buildID string) (map[string]string, error) // name → digest (PEM)
}

// Job describes one finalize run.
type Job struct {
	BuildID     string
	Base        string
	Arch        string // x86_64 | aarch64
	LabelPrefix string // osId
	Version     string
	DiskDigest  string // unsigned disk.raw.zst
	UKIDigest   string // unsigned UKI
	Roothash    string
	EnrollMode  string // force (VMs) | manual (hardware) | off
	Compression imgutil.Compression
	SourceEpoch int64
}

// Output lists produced artifacts (kind → digest).
type Output struct {
	Artifacts map[string]string
	Sizes     map[string]int64
	DiskPath  string            // signed raw disk on local scratch (for conversions/boot tests)
	Payload   map[string]string // update file names (systemd-sysupdate source patterns)
	Roothash  string
}

// Deps are the stage's collaborators.
type Deps struct {
	Blobs   blob.Store
	Signer  Signer
	WorkDir string
	Log     io.Writer
	// Record registers an artifact before the signer is asked to sign it (the
	// signer only signs metadata that belongs to the build). Optional.
	Record func(ctx context.Context, kind, digest string, size int64) error
	// Convert produces download formats (kind → file) from the signed raw
	// disk; they are listed in the signed SHA256SUMS. Optional.
	Convert func(ctx context.Context, disk, dir string) (map[string]string, error)
}

func (d Deps) logf(format string, a ...any) {
	if d.Log != nil {
		fmt.Fprintf(d.Log, "finalize: "+format+"\n", a...)
	}
}

func fetch(ctx context.Context, bs blob.Store, digest, dst string) error {
	r, _, err := bs.Open(ctx, digest)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", digest, err)
	}
	defer r.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func fetchBytes(ctx context.Context, bs blob.Store, digest string, max int64) ([]byte, error) {
	r, _, err := bs.Open(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, max))
}

func typesFor(arch string) (usr, verity, sig gpt.GUID, err error) {
	switch arch {
	case "x86_64":
		return gpt.TypeUsrX86_64, gpt.TypeUsrVerityX86_64, gpt.TypeUsrVeritySigX8664, nil
	case "aarch64":
		return gpt.TypeUsrArm64, gpt.TypeUsrVerityArm64, gpt.TypeUsrVeritySigArm64, nil
	}
	return usr, verity, sig, fmt.Errorf("finalize: unsupported arch %s", arch)
}

var efiArch = map[string]string{"x86_64": "X64", "aarch64": "AA64"}

// Run executes the stage.
func Run(ctx context.Context, j Job, d Deps) (*Output, error) {
	if len(j.Roothash) != 64 {
		return nil, errors.New("finalize: missing root hash")
	}
	usrT, verT, sigT, err := typesFor(j.Arch)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(d.WorkDir, j.BuildID, "finalize")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	stem := j.LabelPrefix + "_" + j.Version
	disk := filepath.Join(dir, stem+".raw")

	d.logf("fetching unsigned disk %s", j.DiskDigest)
	zst := filepath.Join(dir, "unsigned.raw.zst")
	if err := fetch(ctx, d.Blobs, j.DiskDigest, zst); err != nil {
		return nil, err
	}
	if err := imgutil.Unzstd(ctx, zst, disk); err != nil {
		return nil, err
	}
	os.Remove(zst)

	tbl, err := gpt.ReadFile(disk)
	if err != nil {
		return nil, err
	}
	labelA, labelV, labelS := stem, stem+"_v", stem+"_s"
	usrP, ok1 := tbl.ByName(labelA)
	verP, ok2 := tbl.ByName(labelV)
	if !ok1 || !ok2 || usrP.Type != usrT || verP.Type != verT {
		return nil, fmt.Errorf("%w: disk has no A slot partitions %s/%s", ErrIntegrity, labelA, labelV)
	}
	esps := tbl.ByType(gpt.TypeESP)
	if len(esps) != 1 {
		return nil, errors.New("finalize: expected exactly one ESP")
	}
	if _, exists := tbl.ByName(labelS); exists {
		return nil, fmt.Errorf("%w: disk already has a verity signature partition", ErrIntegrity)
	}

	// usrhash= locates /usr and its hash tree by partition UUID: the first and
	// second half of the root hash (Discoverable Partitions convention).
	if usrP.UUID.String() != uuidOf(j.Roothash[:32]) || verP.UUID.String() != uuidOf(j.Roothash[32:]) {
		return nil, fmt.Errorf("%w: slot A partition UUIDs are not derived from the root hash", ErrIntegrity)
	}
	// 1. Verify the usr partition against the root hash before signing it.
	d.logf("verifying usr partition against root hash %s", j.Roothash)
	// Update payload names carry the partition UUID (sysupdate's @u): for usr
	// and usr-verity it is derived from the root hash, as usrhash= expects.
	usrRaw := filepath.Join(dir, stem+"_"+usrP.UUID.String()+".usr.raw")
	verRaw := filepath.Join(dir, stem+"_"+verP.UUID.String()+".usr-verity.raw")
	if err := imgutil.ExtractRange(disk, usrP.Offset(), usrP.Size(), usrRaw); err != nil {
		return nil, err
	}
	if err := imgutil.ExtractRange(disk, verP.Offset(), verP.Size(), verRaw); err != nil {
		return nil, err
	}
	if err := imgutil.Run(ctx, nil, "veritysetup", "verify", usrRaw, verRaw, j.Roothash); err != nil {
		return nil, fmt.Errorf("%w: usr partition does not match root hash: %v", ErrIntegrity, err)
	}

	// 2. Signatures.
	d.logf("signing UKI")
	ukiDigest, err := d.Signer.SignUKI(ctx, j.BuildID, j.UKIDigest)
	if err != nil {
		return nil, fmt.Errorf("sign UKI: %w", err)
	}
	uki := filepath.Join(dir, stem+".efi")
	if err := fetch(ctx, d.Blobs, ukiDigest, uki); err != nil {
		return nil, err
	}
	instDigest, err := d.Signer.InstallerUKI(ctx, j.BuildID, j.UKIDigest)
	if err != nil {
		return nil, fmt.Errorf("sign installer UKI: %w", err)
	}
	instUKI := filepath.Join(dir, InstallerUKIName(j.LabelPrefix, j.Version))
	if err := fetch(ctx, d.Blobs, instDigest, instUKI); err != nil {
		return nil, err
	}
	blDigest, err := d.Signer.Bootloader(ctx, j.BuildID)
	if err != nil {
		return nil, fmt.Errorf("sign bootloader: %w", err)
	}
	bl := filepath.Join(dir, "systemd-boot.efi")
	if err := fetch(ctx, d.Blobs, blDigest, bl); err != nil {
		return nil, err
	}
	sigDigest, err := d.Signer.VeritySig(ctx, j.BuildID, j.Roothash)
	if err != nil {
		return nil, fmt.Errorf("verity signature: %w", err)
	}
	sigPayload, err := fetchBytes(ctx, d.Blobs, sigDigest, 1<<20)
	if err != nil {
		return nil, err
	}
	if len(sigPayload) != 16<<10 || !strings.Contains(string(sigPayload[:200]), j.Roothash) {
		return nil, errors.New("finalize: signer returned an unexpected verity signature")
	}

	// 3. ESP: signed bootloader, signed UKI, enrollment, loader.conf.
	epoch := time.Unix(j.SourceEpoch, 0).UTC()
	fat := imgutil.FAT{Image: disk, Offset: esps[0].Offset(), MTime: epoch}
	a := efiArch[j.Arch]
	oldUKI := "/EFI/Linux/" + stem + ".efi"
	if !fat.Exists(ctx, oldUKI) {
		return nil, fmt.Errorf("finalize: ESP has no %s", oldUKI)
	}
	d.logf("installing signed boot chain into the ESP")
	for _, step := range []struct{ src, dst string }{
		{uki, oldUKI},
		{instUKI, "/EFI/Linux/" + filepath.Base(instUKI)},
		{bl, "/EFI/BOOT/BOOT" + a + ".EFI"},
		{bl, "/EFI/systemd/systemd-boot" + strings.ToLower(a) + ".efi"},
	} {
		if err := fat.Put(ctx, step.src, step.dst); err != nil {
			return nil, err
		}
	}
	if j.EnrollMode != "off" {
		files, err := d.Signer.Enrollment(ctx, j.BuildID)
		if err != nil {
			return nil, fmt.Errorf("enrollment files: %w", err)
		}
		for _, n := range []string{"PK.auth", "KEK.auth", "db.auth"} {
			b, err := fetchBytes(ctx, d.Blobs, files[n], 1<<20)
			if err != nil {
				return nil, err
			}
			if err := fat.PutBytes(ctx, b, "/loader/keys/auto/"+n); err != nil {
				return nil, err
			}
		}
	}
	// The default entry is the OS (any version sysupdate installs later); the
	// installer entry is reachable from the menu (hold Space at boot).
	loader := "timeout 0\nconsole-mode keep\neditor no\ndefault " + j.LabelPrefix + "_*\n"
	if j.EnrollMode == "force" || j.EnrollMode == "manual" {
		loader += "secure-boot-enroll " + j.EnrollMode + "\n"
	}
	if err := fat.PutBytes(ctx, []byte(loader), "/loader/loader.conf"); err != nil {
		return nil, err
	}

	// 4. The deferred verity signature partition, right after slot A's verity.
	sigSectors := uint64(len(sigPayload) / 512)
	first := verP.LastLBA + 1
	if !tbl.Free(first, first+sigSectors-1) {
		return nil, errors.New("finalize: no space reserved for the verity signature partition")
	}
	sigP, err := tbl.Add(gpt.Partition{Type: sigT, UUID: gpt.GUIDFrom("foundry-verity-sig", j.Roothash), FirstLBA: first,
		LastLBA: first + sigSectors - 1, Attrs: verP.Attrs, Name: labelS})
	if err != nil {
		return nil, err
	}
	if err := imgutil.WriteAt(disk, sigP.Offset(), sigPayload); err != nil {
		return nil, err
	}
	// Empty B slots get their (zeroed) signature partition too, so
	// systemd-sysupdate finds a complete usr/usr-verity/usr-verity-sig set.
	for _, ev := range tbl.ByType(verT) {
		if ev.Name != "_empty" {
			continue
		}
		f0 := ev.LastLBA + 1
		if !tbl.Free(f0, f0+sigSectors-1) {
			return nil, errors.New("finalize: no space reserved for an empty slot's verity signature partition")
		}
		if _, err := tbl.Add(gpt.Partition{Type: sigT, UUID: gpt.GUIDFrom("foundry-empty-sig", j.Roothash, ev.UUID.String()), FirstLBA: f0,
			LastLBA: f0 + sigSectors - 1, Attrs: ev.Attrs, Name: "_empty"}); err != nil {
			return nil, err
		}
		if err := imgutil.WriteAt(disk, int64(f0)*512, make([]byte, len(sigPayload))); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := tbl.Write(f); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	// Re-read: the table must parse and slot A must be unchanged.
	if t2, err := gpt.ReadFile(disk); err != nil {
		return nil, err
	} else if p, _ := t2.ByName(labelA); p != usrP {
		return nil, errors.New("finalize: usr partition entry changed")
	}

	// 5. Artifacts.
	sigRaw := filepath.Join(dir, stem+"_"+sigP.UUID.String()+".usr-verity-sig.raw")
	if err := os.WriteFile(sigRaw, sigPayload, 0o644); err != nil {
		return nil, err
	}
	out := &Output{Artifacts: map[string]string{}, Sizes: map[string]int64{}, DiskPath: disk, Roothash: j.Roothash}
	put := func(kind, path string) error {
		dg, n, err := blob.PutFile(ctx, d.Blobs, path)
		if err != nil {
			return fmt.Errorf("store %s: %w", kind, err)
		}
		out.Artifacts[kind], out.Sizes[kind] = dg, n
		return nil
	}
	d.logf("compressing update payload and signed disk")
	usrZst := usrRaw + ".zst"
	if err := imgutil.Zstd(ctx, j.Compression, usrRaw, usrZst); err != nil {
		return nil, err
	}
	diskZst := filepath.Join(dir, stem+".raw.zst")
	if err := imgutil.Zstd(ctx, j.Compression, disk, diskZst); err != nil {
		return nil, err
	}
	// Download formats are produced before SHA256SUMS so the signature covers them.
	conv := map[string]string{}
	if d.Convert != nil {
		d.logf("producing download formats")
		got, err := d.Convert(ctx, disk, dir)
		if err != nil {
			return nil, err
		}
		conv = got
	}
	// SHA256SUMS covers the uncompressed update files (what sysupdate installs)
	// and every download.
	sums := map[string]string{}
	files := []string{usrRaw, verRaw, sigRaw, uki, usrZst, diskZst}
	for _, p := range conv {
		files = append(files, p)
	}
	for _, p := range files {
		dg, _, err := imgutil.SHA256File(p)
		if err != nil {
			return nil, err
		}
		sums[filepath.Base(p)] = strings.TrimPrefix(dg, "sha256:")
	}
	out.Payload = map[string]string{"usr": filepath.Base(usrRaw), "usr-verity": filepath.Base(verRaw), "usr-verity-sig": filepath.Base(sigRaw), "uki": filepath.Base(uki)}
	for kind, p := range map[string]string{"uki": uki, "usr-verity": verRaw, "usr-verity-sig": sigRaw, "usr.zst": usrZst, "disk.raw.zst": diskZst} {
		if err := put(kind, p); err != nil {
			return nil, err
		}
	}
	for kind, p := range conv {
		if err := put(kind, p); err != nil {
			return nil, err
		}
		os.Remove(p)
	}
	os.Remove(usrRaw)
	os.Remove(usrZst)
	os.Remove(diskZst)
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		fmt.Fprintf(&sb, "%s  %s\n", sums[n], n)
	}
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sumsPath, []byte(sb.String()), 0o644); err != nil {
		return nil, err
	}
	if err := put("sha256sums", sumsPath); err != nil {
		return nil, err
	}
	if d.Record != nil {
		if err := d.Record(ctx, "sha256sums", out.Artifacts["sha256sums"], out.Sizes["sha256sums"]); err != nil {
			return nil, err
		}
	}
	sd, err := d.Signer.SignBlob(ctx, j.BuildID, out.Artifacts["sha256sums"])
	if err != nil {
		return nil, fmt.Errorf("sign SHA256SUMS: %w", err)
	}
	out.Artifacts["sha256sums.sig"] = sd
	// Public keys let anyone verify the release offline (SHA256SUMS.sig with
	// the artifact key, Secure Boot signatures with the db certificate).
	if keys, err := d.Signer.PublicKeys(ctx, j.BuildID); err == nil {
		for name, kind := range map[string]string{"artifact.pub.pem": "pubkey-artifact", "db.crt": "pubkey-db"} {
			if dg, ok := keys[name]; ok {
				out.Artifacts[kind] = dg
			}
		}
	}
	d.logf("done: UKI %s, verity-sig %s", short(out.Artifacts["uki"]), short(out.Artifacts["usr-verity-sig"]))
	return out, nil
}

// InstallerUKIName is the ESP file name of the installer UKI. It does not
// match the "<id>_*" default pattern, sysupdate's MatchPattern, or boot counting.
func InstallerUKIName(prefix, version string) string {
	return prefix + "-installer_" + version + ".efi"
}

func short(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func uuidOf(h string) string {
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// DigestBytes hashes in-memory data.
func DigestBytes(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
