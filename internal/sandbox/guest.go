package sandbox

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"foundry.dev/foundry/internal/builder"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/imgutil"
)

// Guest device paths (virtio-blk serials set by the host).
var (
	GuestJobDev     = "/dev/disk/by-id/virtio-job"
	GuestScratchDev = "/dev/disk/by-id/virtio-scratch"
	GuestOutDev     = "/dev/disk/by-id/virtio-out"
	GuestLogPort    = "/dev/virtio-ports/foundry.log"
	GuestWork       = "/work"
)

// dirResolver resolves refs from the unpacked job disk.
type dirResolver struct{ dir string }

func (d dirResolver) Asset(digest string) (io.ReadCloser, error) {
	h := strings.TrimPrefix(digest, "sha256:")
	if strings.ContainsAny(h, "/\\.") {
		return nil, fmt.Errorf("bad asset ref %q", digest)
	}
	return os.Open(filepath.Join(d.dir, "assets", h))
}

func (d dirResolver) Component(name string) (io.ReadCloser, error) {
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("bad component %q", name)
	}
	return os.Open(filepath.Join(d.dir, "components", name))
}

// RunGuest is the build VM's entry point (cmd/builder vm). It never returns
// an error to the caller without first trying to write result.json, so the
// host always learns why a build failed.
func RunGuest(ctx context.Context) error {
	start := time.Now()
	logw := io.Writer(os.Stderr)
	if f, err := os.OpenFile(GuestLogPort, os.O_WRONLY, 0); err == nil {
		defer f.Close()
		logw = io.MultiWriter(f, os.Stderr)
	}
	res := GuestResult{Files: map[string]string{}}
	files := map[string]string{} // name → path
	err := guestBuild(ctx, logw, &res, files)
	res.Seconds = time.Since(start).Seconds()
	if err != nil {
		res.OK, res.Error = false, err.Error()
		fmt.Fprintf(logw, "foundry-builder: FAILED: %v\n", err)
	} else {
		res.OK = true
	}
	if werr := writeOutput(res, files); werr != nil {
		fmt.Fprintf(logw, "foundry-builder: cannot write output disk: %v\n", werr)
		return werr
	}
	return err
}

func guestBuild(ctx context.Context, logw io.Writer, res *GuestResult, files map[string]string) error {
	for _, d := range []string{GuestJobDev, GuestScratchDev, GuestOutDev} { // udev may still be creating links
		for i := 0; ; i++ {
			if _, err := os.Stat(d); err == nil {
				break
			}
			if i == 120 {
				return fmt.Errorf("device %s did not appear", d)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	fmt.Fprintln(logw, "foundry-builder: preparing scratch disk")
	if out, err := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-L", "scratch", GuestScratchDev).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs scratch: %v: %s", err, out)
	}
	os.MkdirAll(GuestWork, 0o755)
	if out, err := exec.CommandContext(ctx, "mount", GuestScratchDev, GuestWork).CombinedOutput(); err != nil {
		return fmt.Errorf("mount scratch: %v: %s", err, out)
	}
	jobDir := filepath.Join(GuestWork, "job")
	if err := untar(GuestJobDev, jobDir); err != nil {
		return fmt.Errorf("job disk: %w", err)
	}
	var spec JobSpec
	b, err := os.ReadFile(filepath.Join(jobDir, "job.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return err
	}
	if spec.SelfTest {
		return selfTest(ctx, logw, spec, res)
	}
	tf, err := os.Open(filepath.Join(jobDir, "tree.tar"))
	if err != nil {
		return err
	}
	tree, err := compiler.ReadTar(tf)
	tf.Close()
	if err != nil {
		return err
	}
	cfg := builder.Config{WorkDir: filepath.Join(GuestWork, "builds"), CacheDir: filepath.Join(GuestWork, "cache"), ToolsTree: "no",
		Proxy: spec.Proxy, Timeout: time.Duration(spec.TimeoutSec) * time.Second}
	r, err := builder.Run(ctx, cfg, builder.Job{BuildID: spec.BuildID, SpecDigest: spec.SpecDigest, Tree: tree, ImageName: spec.ImageName,
		Resolver: dirResolver{dir: jobDir}, Log: logw})
	if err != nil {
		return err
	}
	res.Roothash = r.Roothash
	fmt.Fprintln(logw, "foundry-builder: compressing disk image")
	zst := filepath.Join(GuestWork, spec.ImageName+".raw.zst")
	if err := imgutil.Zstd(ctx, imgutil.Compression{ZstdLevel: max(spec.ZstdLevel, 3)}, r.Artifacts["disk"].Path, zst); err != nil {
		return err
	}
	files["disk.raw.zst"] = zst
	res.Files["disk.zst"] = "disk.raw.zst"
	for kind, name := range map[string]string{"uki": "uki.efi", "manifest-mkosi": "manifest.json", "roothash": "roothash", "pkgdb": "pkgdb.md5sums"} {
		if a, ok := r.Artifacts[kind]; ok {
			files[name] = a.Path
			res.Files[kind] = name
		}
	}
	return nil
}

func selfTest(ctx context.Context, logw io.Writer, spec JobSpec, res *GuestResult) error {
	fmt.Fprintln(logw, "foundry-builder: self-test")
	report := []string{"vm=up"}
	if spec.Proxy != "" {
		pu, _ := url.Parse(spec.Proxy)
		c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
		for _, target := range []string{"http://archive.ubuntu.com/ubuntu/dists/noble/Release", "http://example.com/"} {
			resp, err := c.Get(target)
			status := "error"
			if err == nil {
				status = fmt.Sprint(resp.StatusCode)
				resp.Body.Close()
			}
			report = append(report, target+"="+status)
		}
		// direct (non-proxied) egress must be impossible
		d := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
		if resp, err := d.Get("http://archive.ubuntu.com/"); err == nil {
			resp.Body.Close()
			report = append(report, "direct=reachable")
		} else {
			report = append(report, "direct=blocked")
		}
	}
	if _, err := exec.LookPath("mkosi"); err == nil {
		report = append(report, "mkosi=present")
	}
	res.SelfTest = strings.Join(report, " ")
	fmt.Fprintln(logw, "foundry-builder: "+res.SelfTest)
	return nil
}

func untar(dev, dir string) error {
	f, err := os.Open(dev)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(h.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsafe job entry %q", h.Name)
		}
		p := filepath.Join(dir, clean)
		os.MkdirAll(filepath.Dir(p), 0o755)
		out, err := os.Create(p)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
}

// writeOutput writes result.json and the files as a tar stream onto the
// raw output disk.
func writeOutput(res GuestResult, files map[string]string) error {
	f, err := os.OpenFile(GuestOutDev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for name, path := range files {
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		st, _ := src.Stat()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: st.Size(), Typeflag: tar.TypeReg}); err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	js, _ := json.Marshal(res)
	if err := tw.WriteHeader(&tar.Header{Name: "result.json", Mode: 0o600, Size: int64(len(js)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	tw.Write(js)
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// ErrNotInVM is returned when guest devices are missing.
var ErrNotInVM = errors.New("sandbox: not running inside a build VM")
