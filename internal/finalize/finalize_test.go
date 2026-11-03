package finalize

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/gpt"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/signer" // test-only: stands in for cmd/signer
)

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, x := range tools {
		if _, err := exec.LookPath(x); err != nil {
			t.Skipf("%s not installed", x)
		}
	}
	for _, p := range []string{"/usr/lib/systemd/boot/efi/linuxx64.efi.stub", "/usr/lib/systemd/systemd-measure"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s missing", p)
		}
	}
}

func sh(t *testing.T, stdin string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "MTOOLS_SKIP_CHECK=1")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// localSigner implements Signer with in-process keys (what cmd/signer does
// after authorization).
type localSigner struct {
	t     *testing.T
	keys  *signer.Keys
	tools signer.Tools
	bs    blob.Store
	dir   string
	calls []string
}

func (l *localSigner) put(p string) string {
	d, _, err := blob.PutFile(context.Background(), l.bs, p)
	if err != nil {
		l.t.Fatal(err)
	}
	return d
}
func (l *localSigner) putB(b []byte) string {
	d, _, _ := l.bs.Put(context.Background(), bytes.NewReader(b), "", 0)
	return d
}
func (l *localSigner) SignUKI(ctx context.Context, id, dg string) (string, error) {
	l.calls = append(l.calls, "uki")
	p, _ := l.bs.Path(dg)
	out := filepath.Join(l.dir, "signed.efi")
	if _, err := l.keys.BuildUKI(ctx, l.tools, "ubuntu-24.04", p, out); err != nil {
		return "", err
	}
	return l.put(out), nil
}
func (l *localSigner) InstallerUKI(ctx context.Context, id, dg string) (string, error) {
	l.calls = append(l.calls, "installer-uki")
	p, _ := l.bs.Path(dg)
	out := filepath.Join(l.dir, "installer.efi")
	if _, err := l.keys.BuildUKI(ctx, l.tools, "ubuntu-24.04", p, out, signer.VariantInstaller); err != nil {
		return "", err
	}
	return l.put(out), nil
}
func (l *localSigner) Bootloader(ctx context.Context, id string) (string, error) {
	out := filepath.Join(l.dir, "bl.efi")
	if err := l.keys.SignBootloader(ctx, l.tools, "ubuntu-24.04", out); err != nil {
		return "", err
	}
	return l.put(out), nil
}
func (l *localSigner) VeritySig(ctx context.Context, id, rh string) (string, error) {
	b, err := l.keys.VeritySig(ctx, l.tools, rh)
	return l.putB(b), err
}
func (l *localSigner) Enrollment(ctx context.Context, id string) (map[string]string, error) {
	f, err := l.keys.EnrollmentFiles(ctx, l.tools)
	out := map[string]string{}
	for n, b := range f {
		out[n] = l.putB(b)
	}
	return out, err
}
func (l *localSigner) SignBlob(ctx context.Context, id, dg string) (string, error) {
	r, _, _ := l.bs.Open(ctx, dg)
	defer r.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
	s, err := l.keys.SignBlob(buf.Bytes())
	return l.putB(s), err
}
func (l *localSigner) PublicKeys(ctx context.Context, id string) (map[string]string, error) {
	return nil, nil
}

// makeDisk builds a miniature image shaped like a mkosi output: ESP with an
// unsigned UKI, usr A (+verity, gap for the sig), empty B slots.
func makeDisk(t *testing.T, dir string) (disk, roothash, uki string) {
	usr := filepath.Join(dir, "usr.img")
	f, _ := os.Create(usr)
	f.Truncate(8 << 20)
	f.WriteAt(bytes.Repeat([]byte("usr-data"), 1000), 4096)
	f.Close()
	ver := filepath.Join(dir, "verity.img")
	out, err := exec.Command("veritysetup", "format", usr, ver).CombinedOutput()
	if err != nil {
		t.Fatalf("veritysetup: %v %s", err, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "Root hash:") {
			roothash = strings.TrimSpace(strings.TrimPrefix(l, "Root hash:"))
		}
	}
	vst, _ := os.Stat(ver)
	verSectors := (vst.Size() + 511) / 512
	os.WriteFile(filepath.Join(dir, "linux"), bytes.Repeat([]byte("K"), 4096), 0o644)
	uki = filepath.Join(dir, "unsigned.efi")
	sh(t, "", "ukify", "build", "--linux="+filepath.Join(dir, "linux"), "--cmdline=quiet usrhash="+roothash,
		"--uname=6.8.0-t", "--os-release=ID=foundry\n", "--output="+uki)
	esp := filepath.Join(dir, "esp.img")
	ef, _ := os.Create(esp)
	ef.Truncate(32 << 20)
	ef.Close()
	sh(t, "", "mkfs.vfat", "-F", "32", "-n", "ESP", esp)
	sh(t, "", "mmd", "-i", esp, "::/EFI", "::/EFI/Linux", "::/EFI/BOOT", "::/EFI/systemd", "::/loader")
	sh(t, "", "mcopy", "-i", esp, uki, "::/EFI/Linux/os_1.0.efi")
	sh(t, "", "mcopy", "-i", esp, uki, "::/EFI/BOOT/BOOTX64.EFI")

	disk = filepath.Join(dir, "disk.raw")
	df, _ := os.Create(disk)
	df.Truncate(80 << 20)
	df.Close()
	espStart, usrStart := int64(2048), int64(2048+65536)
	verStart := usrStart + 16384
	gap := verStart + verSectors
	layout := "label: gpt\n" +
		"start=2048, size=65536, type=" + gpt.TypeESP.String() + ", name=esp\n" +
		"start=" + itoa(usrStart) + ", size=16384, type=" + gpt.TypeUsrX86_64.String() + ", uuid=" + uuidOf(roothash[:32]) + ", name=os_1.0, attrs=\"GUID:60\"\n" +
		"start=" + itoa(verStart) + ", size=" + itoa(verSectors) + ", type=" + gpt.TypeUsrVerityX86_64.String() + ", uuid=" + uuidOf(roothash[32:]) + ", name=os_1.0_v, attrs=\"GUID:60\"\n" +
		"start=" + itoa(gap+32) + ", size=16384, type=" + gpt.TypeUsrX86_64.String() + ", name=_empty\n" +
		"start=" + itoa(gap+32+16384) + ", size=" + itoa(verSectors) + ", type=" + gpt.TypeUsrVerityX86_64.String() + ", name=_empty\n"
	sh(t, layout, "sfdisk", "-q", disk)
	for _, c := range []struct {
		src string
		off int64
	}{{esp, espStart}, {usr, usrStart}, {ver, verStart}} {
		sh(t, "", "dd", "if="+c.src, "of="+disk, "bs=512", "seek="+itoa(c.off), "conv=notrunc,sparse", "status=none")
	}
	return disk, roothash, uki
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestFinalizeEndToEnd(t *testing.T) {
	need(t, "sbsign", "ukify", "veritysetup", "sfdisk", "mkfs.vfat", "mcopy", "zstd", "cert-to-efi-sig-list", "sign-efi-sig-list", "openssl")
	ctx := context.Background()
	dir := t.TempDir()
	disk, rh, uki := makeDisk(t, dir)
	bs, _ := blob.NewFS(filepath.Join(dir, "blobs"))
	zst := disk + ".zst"
	if err := imgutil.Zstd(ctx, imgutil.Compression{ZstdLevel: 3}, disk, zst); err != nil {
		t.Fatal(err)
	}
	diskD, _, _ := blob.PutFile(ctx, bs, zst)
	ukiD, _, _ := blob.PutFile(ctx, bs, uki)
	keys, _ := signer.Open(filepath.Join(dir, "keys"), "acme", true)
	ls := &localSigner{t: t, keys: keys, tools: signer.DefaultTools(), bs: bs, dir: dir}
	job := Job{BuildID: "bld_x", Base: "ubuntu-24.04", Arch: "x86_64", LabelPrefix: "os", Version: "1.0",
		DiskDigest: diskD, UKIDigest: ukiD, Roothash: rh, EnrollMode: "force", Compression: imgutil.Compression{ZstdLevel: 3}, SourceEpoch: 1767225600}
	var log bytes.Buffer
	out, err := Run(ctx, job, Deps{Blobs: bs, Signer: ls, WorkDir: filepath.Join(dir, "work"), Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for _, k := range []string{"uki", "usr-verity", "usr-verity-sig", "usr.zst", "disk.raw.zst", "sha256sums", "sha256sums.sig"} {
		if out.Artifacts[k] == "" {
			t.Errorf("missing artifact %s", k)
		}
	}
	// GPT has the signature partition right after verity, with the payload in it.
	tbl, err := gpt.ReadFile(out.DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := tbl.ByName("os_1.0_v")
	s, ok := tbl.ByName("os_1.0_s")
	if !ok || s.FirstLBA != v.LastLBA+1 || s.Type != gpt.TypeUsrVeritySigX8664 || s.Size() != 16<<10 {
		t.Fatalf("sig partition %+v", s)
	}
	if sigs := tbl.ByType(gpt.TypeUsrVeritySigX8664); len(sigs) != 2 || sigs[0].Name != "_empty" && sigs[1].Name != "_empty" {
		t.Fatalf("B slot signature partition missing: %+v", sigs)
	}
	f, _ := os.Open(out.DiskPath)
	payload := make([]byte, 16<<10)
	f.ReadAt(payload, s.Offset())
	f.Close()
	if got, err := keys.VerifyVeritySig(ctx, signer.DefaultTools(), payload); err != nil || got != rh {
		t.Fatalf("verity sig: %v %s", err, got)
	}
	// ESP: signed UKI and bootloader, enrollment keys, loader.conf.
	esp := tbl.ByType(gpt.TypeESP)[0]
	fat := imgutil.FAT{Image: out.DiskPath, Offset: esp.Offset()}
	for _, p := range []string{"/EFI/Linux/os_1.0.efi", "/EFI/BOOT/BOOTX64.EFI", "/EFI/systemd/systemd-bootx64.efi"} {
		b, err := fat.Get(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(dir, "chk.efi")
		os.WriteFile(tmp, b, 0o644)
		if err := keys.VerifyPE(ctx, signer.DefaultTools(), tmp); err != nil {
			t.Errorf("%s not signed: %v", p, err)
		}
	}
	lc, _ := fat.Get(ctx, "/loader/loader.conf")
	if !strings.Contains(string(lc), "secure-boot-enroll force") {
		t.Fatalf("loader.conf: %s", lc)
	}
	if !fat.Exists(ctx, "/loader/keys/auto/db.auth") {
		t.Fatal("enrollment files missing")
	}
	// Update payload names carry the root-hash-derived partition UUIDs.
	wantUsr := "os_1.0_" + rh[0:8] + "-" + rh[8:12] + "-" + rh[12:16] + "-" + rh[16:20] + "-" + rh[20:32] + ".usr.raw"
	if out.Payload["usr"] != wantUsr || !strings.HasPrefix(out.Payload["usr-verity"], "os_1.0_"+rh[32:40]+"-") {
		t.Fatalf("payload names: %v (want usr %s)", out.Payload, wantUsr)
	}
	// SHA256SUMS is signed by the artifact key.
	sums, _ := os.ReadFile(mustPath(t, bs, out.Artifacts["sha256sums"]))
	sig, _ := os.ReadFile(mustPath(t, bs, out.Artifacts["sha256sums.sig"]))
	pub, _ := os.ReadFile(filepath.Join(keys.Dir, "artifact.pub.pem"))
	if !signer.VerifyBlob(pub, sums, sig) || !strings.Contains(string(sums), ".usr-verity-sig.raw") {
		t.Fatalf("SHA256SUMS: %s", sums)
	}
	// Running again on an already-finalized disk is refused.
	job2 := job
	job2.DiskDigest, _, _ = blob.PutFile(ctx, bs, func() string {
		z := filepath.Join(dir, "signed.zst")
		imgutil.Zstd(ctx, imgutil.Compression{ZstdLevel: 3}, out.DiskPath, z)
		return z
	}())
	if _, err := Run(ctx, job2, Deps{Blobs: bs, Signer: ls, WorkDir: filepath.Join(dir, "work2")}); err == nil || !strings.Contains(err.Error(), "already has") {
		t.Fatalf("double finalize: %v", err)
	}
	// A wrong root hash is caught before anything is signed.
	n := len(ls.calls)
	job3 := job
	job3.Roothash = strings.Repeat("0", 64)
	if _, err := Run(ctx, job3, Deps{Blobs: bs, Signer: ls, WorkDir: filepath.Join(dir, "work3")}); err == nil || len(ls.calls) != n {
		t.Fatalf("bad roothash: %v (sign calls %d→%d)", err, n, len(ls.calls))
	}
}

func mustPath(t *testing.T, bs blob.Store, d string) string {
	p, ok := bs.Path(d)
	if !ok {
		t.Fatal("no path")
	}
	return p
}
