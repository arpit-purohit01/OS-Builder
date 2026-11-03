package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/scan"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/upload"
)

// imageScan analyses the built /usr against the package snapshot (P3-03/04)
// and stores the file manifest as a report artifact.
func (w *Worker) imageScan(ctx context.Context, b *store.Build, spec *osspec.Spec, dir string, logf func(string)) ([]scan.Finding, error) {
	var out []scan.Finding
	// 1. Package snapshot, checked against the digest the build logged before
	//    running customer content.
	var db scan.PkgDB
	pd, err := w.artifact(ctx, b, "pkgdb")
	if err != nil {
		out = append(out, scan.Finding{Rule: "SCAN-NOPKGDB", Feature: "modified-binary", Severity: scan.SevHigh, Scanner: "diff",
			Detail: "the build produced no package snapshot; package files cannot be verified"})
		db = scan.PkgDB{}
	} else {
		raw, err := w.readBlob(ctx, pd, 256<<20)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		line, lerr := w.DB.LogMatch(ctx, b.ID, "foundry: package snapshot sha256 ")
		if lerr != nil || !strings.HasSuffix(strings.TrimSpace(line), got) {
			out = append(out, scan.Finding{Rule: "SCAN-TAMPER", Severity: scan.SevBlocker, Scanner: "diff",
				Detail: "the package snapshot does not match the digest logged before customer content ran"})
		}
		if db, err = scan.ParsePkgDB(raw); err != nil {
			return nil, err
		}
	}
	// 2. The unsigned disk → /usr extracted in user space.
	dd, err := w.artifact(ctx, b, "disk-unsigned.raw.zst")
	if err != nil {
		return nil, err
	}
	src, release, err := blob.Local(ctx, w.Blobs, dd, dir)
	if err != nil {
		return nil, fmt.Errorf("scan: fetch disk: %w", err)
	}
	defer release()
	disk := filepath.Join(dir, "disk.raw")
	if err := imgutil.Unzstd(ctx, src, disk); err != nil {
		return nil, err
	}
	root, err := scan.ExtractUsr(ctx, disk, dir)
	os.Remove(disk)
	if err != nil {
		return nil, fmt.Errorf("scan: extract /usr: %w", err)
	}
	defer os.RemoveAll(root)
	man, err := scan.BuildManifest(root, "/usr")
	if err != nil {
		return nil, err
	}
	logf(fmt.Sprintf("foundry: image manifest: %d entries", len(man)))
	// 3. What the platform generated (compiled tree) and what came from overlays.
	expected := map[string]bool{}
	if rc, _, err := w.Blobs.Open(ctx, b.TreeDigest); err == nil {
		if t, err := compiler.ReadTar(rc); err == nil {
			for p, f := range t.Files {
				if strings.HasPrefix(p, "mkosi.extra/") && f.AssetRef == "" {
					expected["/"+strings.TrimPrefix(p, "mkosi.extra/")] = true
				}
			}
		}
		rc.Close()
	}
	overlay := map[string]string{}
	customer := map[string]bool{}
	if spec.Expert != nil {
		for i, d := range spec.Expert.Overlays {
			for _, p := range overlayPaths(ctx, w, d) {
				overlay[p] = fmt.Sprintf("overlay %d", i+1)
			}
		}
	}
	if spec.Apps != nil {
		for _, d := range spec.Apps.Custom {
			if a, err := w.DB.GetAsset(ctx, b.OrgID, d); err == nil {
				var rep upload.Report
				json.Unmarshal(a.Validation, &rep)
				if rep.Package != "" {
					customer[rep.Package] = true
				}
			}
		}
	}
	findings, sum := scan.AnalyzeImage(scan.ImageInput{Manifest: man, PkgDB: db, Expected: func(p string) bool { return expected[p] },
		CustomerPkgs: customer, Overlay: overlay, Read: func(p string) ([]byte, error) {
			return os.ReadFile(filepath.Join(root, strings.TrimPrefix(p, "/usr")))
		}})
	logf(fmt.Sprintf("foundry: files %d: %d from packages, %d changed, %d generated, %d added, %d setuid", sum.Files, sum.Owned, sum.Modified, sum.Generated, sum.Unowned, sum.Setuid))
	out = append(out, findings...)
	// 4. File manifest (reviewers diff it against the previous release).
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, e := range man {
		enc.Encode(e)
	}
	zw.Close()
	if _, err := w.putBytes(ctx, b, "file-manifest", buf.Bytes(), false); err != nil {
		return nil, err
	}
	return out, nil
}

// overlayPaths lists the files an overlay archive installs (as / paths).
func overlayPaths(ctx context.Context, w *Worker, digest string) []string {
	rc, _, err := w.Blobs.Open(ctx, digest)
	if err != nil {
		return nil
	}
	defer rc.Close()
	var r io.Reader = rc
	head := make([]byte, 2)
	if _, err := io.ReadFull(rc, head); err != nil {
		return nil
	}
	r = io.MultiReader(bytes.NewReader(head), rc)
	if head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil
		}
		r = gz
	}
	tr := tar.NewReader(r)
	var out []string
	for {
		h, err := tr.Next()
		if err != nil {
			return out
		}
		p := path.Clean("/" + h.Name)
		// Overlays land in the image root; /etc ends up in the first-boot skeleton.
		out = append(out, p)
	}
}
