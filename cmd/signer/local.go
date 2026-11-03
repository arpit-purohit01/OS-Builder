package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/finalize"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/signer"
)

// localSigner implements finalize.Signer in-process (offline signing of a
// locally built image; no database authorization).
type localSigner struct {
	keys  *signer.Keys
	tools signer.Tools
	bs    blob.Store
	base  string
	tmp   string
}

func (l *localSigner) path(d string) (string, error) {
	p, ok := l.bs.Path(d)
	if !ok {
		return "", fmt.Errorf("blob %s not local", d)
	}
	return p, nil
}

func (l *localSigner) putFile(ctx context.Context, p string) (string, error) {
	d, _, err := blob.PutFile(ctx, l.bs, p)
	return d, err
}

func (l *localSigner) putBytes(ctx context.Context, b []byte) (string, error) {
	d, _, err := l.bs.Put(ctx, bytes.NewReader(b), "", 0)
	return d, err
}

func (l *localSigner) SignUKI(ctx context.Context, _, digest string) (string, error) {
	in, err := l.path(digest)
	if err != nil {
		return "", err
	}
	out := filepath.Join(l.tmp, "signed.efi")
	if _, err := l.keys.BuildUKI(ctx, l.tools, l.base, in, out); err != nil {
		return "", err
	}
	return l.putFile(ctx, out)
}

func (l *localSigner) InstallerUKI(ctx context.Context, _, digest string) (string, error) {
	in, err := l.path(digest)
	if err != nil {
		return "", err
	}
	out := filepath.Join(l.tmp, "installer.efi")
	if _, err := l.keys.BuildUKI(ctx, l.tools, l.base, in, out, signer.VariantInstaller); err != nil {
		return "", err
	}
	return l.putFile(ctx, out)
}

func (l *localSigner) Bootloader(ctx context.Context, _ string) (string, error) {
	out := filepath.Join(l.tmp, "boot.efi")
	if err := l.keys.SignBootloader(ctx, l.tools, l.base, out); err != nil {
		return "", err
	}
	return l.putFile(ctx, out)
}

func (l *localSigner) VeritySig(ctx context.Context, _, rh string) (string, error) {
	b, err := l.keys.VeritySig(ctx, l.tools, rh)
	if err != nil {
		return "", err
	}
	return l.putBytes(ctx, b)
}

func (l *localSigner) Enrollment(ctx context.Context, _ string) (map[string]string, error) {
	files, err := l.keys.EnrollmentFiles(ctx, l.tools)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n, b := range files {
		if out[n], err = l.putBytes(ctx, b); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l *localSigner) SignBlob(ctx context.Context, _, digest string) (string, error) {
	r, _, err := l.bs.Open(ctx, digest)
	if err != nil {
		return "", err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return "", err
	}
	sig, err := l.keys.SignBlob(data)
	if err != nil {
		return "", err
	}
	return l.putBytes(ctx, sig)
}

func (l *localSigner) PublicKeys(ctx context.Context, _ string) (map[string]string, error) {
	return map[string]string{}, nil
}

func signImage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sign-image", flag.ExitOnError)
	keysDir := fs.String("keys", env("FOUNDRY_SIGNER_KEYS", "/var/lib/foundry/keys"), "key directory")
	tenant := fs.String("tenant", "dev", "tenant key set")
	disk := fs.String("disk", "", "unsigned disk image (.raw)")
	uki := fs.String("uki", "", "unsigned UKI (.efi)")
	roothash := fs.String("roothash", "", "usr verity root hash (hex or @file)")
	label := fs.String("label", "", "label prefix_version (default: from disk file name)")
	base := fs.String("base", "ubuntu-24.04", "base")
	enroll := fs.String("enroll", "force", "Secure Boot key enrollment: force|manual|off")
	out := fs.String("out", ".", "output directory")
	zlevel := fs.Int("zstd", 3, "zstd level for outputs")
	extraDB := fs.String("extra-db", "", "extra db certificates (comma separated; e.g. Microsoft UEFI CA)")
	fs.Parse(args)
	if *disk == "" || *uki == "" || *roothash == "" {
		return fmt.Errorf("-disk, -uki and -roothash are required")
	}
	rh := *roothash
	if p, ok := strings.CutPrefix(rh, "@"); ok {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rh = strings.TrimSpace(string(b))
	}
	stem := *label
	if stem == "" {
		stem = strings.TrimSuffix(filepath.Base(*disk), ".raw")
	}
	i := strings.LastIndex(stem, "_")
	if i < 0 {
		return fmt.Errorf("label %q is not prefix_version", stem)
	}
	k, err := signer.Open(*keysDir, *tenant, true)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(*out, ".sign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	bs, err := blob.NewFS(filepath.Join(work, "blobs"))
	if err != nil {
		return err
	}
	comp := imgutil.Compression{ZstdLevel: *zlevel, XzLevel: 6}
	zst := filepath.Join(work, "disk.raw.zst")
	if err := imgutil.Zstd(ctx, comp, *disk, zst); err != nil {
		return err
	}
	dd, _, err := blob.PutFile(ctx, bs, zst)
	if err != nil {
		return err
	}
	os.Remove(zst)
	ud, _, err := blob.PutFile(ctx, bs, *uki)
	if err != nil {
		return err
	}
	ls := &localSigner{keys: k, tools: tools(*extraDB), bs: bs, base: *base, tmp: work}
	res, err := finalize.Run(ctx, finalize.Job{BuildID: "local", Base: *base, Arch: "x86_64", LabelPrefix: stem[:i], Version: stem[i+1:],
		DiskDigest: dd, UKIDigest: ud, Roothash: rh, EnrollMode: *enroll, Compression: comp},
		finalize.Deps{Blobs: bs, Signer: ls, WorkDir: work, Log: os.Stderr})
	if err != nil {
		return err
	}
	signed := filepath.Join(*out, stem+".signed.raw")
	if err := os.Rename(res.DiskPath, signed); err != nil {
		return err
	}
	names := map[string]string{"uki": stem + ".efi", "usr-verity-sig": stem + ".usr-verity-sig.raw", "sha256sums": "SHA256SUMS", "sha256sums.sig": "SHA256SUMS.sig"}
	for kind, name := range names {
		p, _ := bs.Path(res.Artifacts[kind])
		if err := copyFile(p, filepath.Join(*out, name)); err != nil {
			return err
		}
	}
	// Update payload (what systemd-sysupdate installs into the B slot).
	upd := filepath.Join(*out, "update")
	if err := os.MkdirAll(upd, 0o755); err != nil {
		return err
	}
	for kind, name := range res.Payload {
		src := res.Artifacts[kind]
		if kind == "usr" {
			p, _ := bs.Path(res.Artifacts["usr.zst"])
			if err := imgutil.Unzstd(ctx, p, filepath.Join(upd, name)); err != nil {
				return err
			}
			continue
		}
		p, _ := bs.Path(src)
		if err := copyFile(p, filepath.Join(upd, name)); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"disk": signed, "update": upd, "payload": res.Payload, "artifacts": res.Artifacts, "keys": k.Dir}, "", "  ")
	fmt.Println(string(b))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	o, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(o, in); err != nil {
		o.Close()
		return err
	}
	return o.Close()
}
