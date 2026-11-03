// Package builder runs one image build: it materializes a compiled tree,
// adds the platform drop-in, invokes mkosi and collects artifacts.
//
// In production this runs inside an ephemeral build VM (cmd/builder). The
// same code runs in "process" sandbox mode for development and CI hosts
// without /dev/kvm; see docs/context/build-pipeline.md §4.
package builder

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"foundry.dev/foundry/internal/procreg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"foundry.dev/foundry/internal/compiler"
)

// Config is host/platform configuration for builds (not spec-derived).
type Config struct {
	WorkDir          string        // per-build directories are created below
	CacheDir         string        // mkosi package + incremental cache (L1, shared across builds)
	ToolsTree        string        // "default" or path to a prebuilt tools tree
	ToolsTreeDist    string        // e.g. "ubuntu"
	ToolsTreeRelease string        // e.g. "resolute"
	ToolsTreeMirror  string        // mirror for the tools tree
	Mkosi            string        // mkosi binary
	Timeout          time.Duration // wall clock limit for mkosi
	KeepWorkspace    bool
	Env              []string // extra environment for mkosi
	DropProxyEnv     bool     // remove *_proxy variables (mirrors reached directly)
	// Proxy routes all package downloads through the egress proxy
	// (http://user:pass@host:port); implies dropping inherited proxy variables.
	Proxy string
	// Procs records mkosi's process group for the crash reaper.
	Procs procreg.Dir
}

// Job describes one build.
type Job struct {
	BuildID    string
	SpecDigest string
	Tree       *compiler.Tree
	ImageName  string // <ImageId>_<ImageVersion>, the stem of mkosi's outputs
	Resolver   compiler.Resolver
	Log        io.Writer // receives line-oriented build log (already redacted)
}

// Artifact is one produced file.
type Artifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Result of a successful build.
type Result struct {
	Dir        string              `json:"dir"`
	Artifacts  map[string]Artifact `json:"artifacts"`
	Roothash   string              `json:"roothash"`
	Packages   []Package           `json:"packages"`
	Duration   time.Duration       `json:"duration"`
	TreeDigest string              `json:"treeDigest"`
}

// ImageNameFromTree reads ImageId/ImageVersion from the tree's mkosi.conf.
func ImageNameFromTree(t *compiler.Tree) (string, error) {
	f, ok := t.Files["mkosi.conf"]
	if !ok {
		return "", errors.New("builder: tree has no mkosi.conf")
	}
	var id, ver string
	for _, l := range strings.Split(string(f.Data), "\n") {
		if v, ok := strings.CutPrefix(l, "ImageId="); ok {
			id = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, "ImageVersion="); ok {
			ver = strings.TrimSpace(v)
		}
	}
	if id == "" || ver == "" {
		return "", errors.New("builder: mkosi.conf lacks ImageId/ImageVersion")
	}
	return id + "_" + ver, nil
}

// Package from the mkosi manifest.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"architecture"`
	Type    string `json:"type"`
}

// ErrBuildFailed wraps mkosi failures (not retryable: spec/package problems).
var ErrBuildFailed = errors.New("image build failed")

// PlatformDropin renders mkosi.conf.d/00-platform.conf.
func PlatformDropin(cfg Config, cacheDir, outDir string) string {
	var w strings.Builder
	w.WriteString("# Platform settings (not spec-derived)\n[Build]\n")
	tt := cfg.ToolsTree
	if tt == "" {
		tt = "default"
	}
	fmt.Fprintf(&w, "ToolsTree=%s\n", tt)
	if tt == "default" {
		if cfg.ToolsTreeDist != "" {
			fmt.Fprintf(&w, "ToolsTreeDistribution=%s\n", cfg.ToolsTreeDist)
		}
		if cfg.ToolsTreeRelease != "" {
			fmt.Fprintf(&w, "ToolsTreeRelease=%s\n", cfg.ToolsTreeRelease)
		}
		if cfg.ToolsTreeMirror != "" {
			fmt.Fprintf(&w, "ToolsTreeMirror=%s\n", cfg.ToolsTreeMirror)
		}
	}
	fmt.Fprintf(&w, "CacheDirectory=%s\nIncremental=yes\n", cacheDir)
	fmt.Fprintf(&w, "\n[Output]\nOutputDirectory=%s\n", outDir)
	return w.String()
}

// Run executes the build. ctx cancellation kills mkosi.
func Run(ctx context.Context, cfg Config, job Job) (*Result, error) {
	start := time.Now()
	if job.Tree == nil || job.ImageName == "" {
		return nil, fmt.Errorf("builder: no compiled tree")
	}
	dir := filepath.Join(cfg.WorkDir, job.BuildID)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	project := filepath.Join(dir, "project")
	out := filepath.Join(dir, "out")
	if err := job.Tree.WriteDir(project, job.Resolver); err != nil {
		return nil, fmt.Errorf("builder: write tree: %w", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	cache := cfg.CacheDir
	if cache == "" {
		cache = filepath.Join(cfg.WorkDir, "cache")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(project, "mkosi.conf.d"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(project, "mkosi.conf.d/00-platform.conf"), []byte(PlatformDropin(cfg, cache, out)), 0o644); err != nil {
		return nil, err
	}
	logw := job.Log
	if logw == nil {
		logw = io.Discard
	}
	fmt.Fprintf(logw, "foundry: build %s spec %s tree %s\n", job.BuildID, job.SpecDigest, job.Tree.Digest())

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Minute
	}
	mctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	mk := cfg.Mkosi
	if mk == "" {
		mk = "mkosi"
	}
	cmd := exec.CommandContext(mctx, mk, "--directory", project, "--force", "build")
	cmd.Dir = project
	env := os.Environ()
	if cfg.DropProxyEnv || cfg.Proxy != "" {
		kept := env[:0]
		for _, e := range env {
			if k := strings.ToLower(strings.SplitN(e, "=", 2)[0]); strings.HasSuffix(k, "_proxy") {
				continue
			}
			kept = append(kept, e)
		}
		env = kept
	}
	if cfg.Proxy != "" {
		env = append(env, "http_proxy="+cfg.Proxy, "https_proxy="+cfg.Proxy, "HTTP_PROXY="+cfg.Proxy, "HTTPS_PROXY="+cfg.Proxy, "no_proxy=", "NO_PROXY=")
	}
	cmd.Env = append(env, cfg.Env...)
	// mkosi spawns helpers (systemd-repart, nspawn, apt); cancellation must stop
	// the whole process group, not just the Python parent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var wg sync.WaitGroup
	wg.Add(1)
	var tail []string
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			line := Redact(sc.Text())
			fmt.Fprintln(logw, line)
			tail = append(tail, line)
			if len(tail) > 40 {
				tail = tail[1:]
			}
		}
		io.Copy(io.Discard, pr)
	}()
	if err := cmd.Start(); err != nil {
		pw.Close()
		wg.Wait()
		return nil, err
	}
	unreg := cfg.Procs.Register("mkosi-"+job.BuildID, cmd.Process.Pid)
	err := cmd.Wait()
	if mctx.Err() != nil && cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // stragglers that ignored SIGTERM
	}
	unreg()
	pw.Close()
	wg.Wait()
	if mctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w: timed out after %s", ErrBuildFailed, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: mkosi: %v\n%s", ErrBuildFailed, err, strings.Join(tail, "\n"))
	}
	res, err := collect(out, job.ImageName)
	if err != nil {
		return nil, err
	}
	res.Dir = dir
	res.Duration = time.Since(start)
	res.TreeDigest = job.Tree.Digest()
	if !cfg.KeepWorkspace {
		os.RemoveAll(project)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
	return res, nil
}

// collect maps mkosi outputs to artifact kinds.
func collect(out string, stem string) (*Result, error) {
	ents, err := os.ReadDir(out)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(out, stem+".raw")); err != nil {
		return nil, fmt.Errorf("builder: no disk image %s.raw in %s", stem, out)
	}
	_ = ents
	want := map[string]string{
		"disk":           stem + ".raw",
		"uki":            stem + ".efi",
		"usr-verity":     stem + ".usr-verity.raw",
		"usr-verity-sig": stem + ".usr-verity-sig.raw",
		"esp":            stem + ".esp.raw",
		"manifest-mkosi": stem + ".manifest",
		"roothash":       stem + ".roothash",
		"kernel":         stem + ".vmlinuz",
		"initrd":         stem + ".initrd",
		"os-release":     stem + ".osrelease",
		"pkgdb":          "foundry-dpkg.md5sums",
	}
	res := &Result{Artifacts: map[string]Artifact{}}
	kinds := make([]string, 0, len(want))
	for k := range want {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		p := filepath.Join(out, want[kind])
		st, err := os.Stat(p)
		if err != nil {
			if kind == "disk" || kind == "uki" || kind == "roothash" {
				return nil, fmt.Errorf("builder: missing required output %s", want[kind])
			}
			continue
		}
		sum, err := FileSHA256(p)
		if err != nil {
			return nil, err
		}
		res.Artifacts[kind] = Artifact{Kind: kind, Path: p, Size: st.Size(), SHA256: sum}
	}
	if b, err := os.ReadFile(filepath.Join(out, want["roothash"])); err == nil {
		res.Roothash = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(out, want["manifest-mkosi"])); err == nil {
		var m struct {
			Packages []Package `json:"packages"`
		}
		if json.Unmarshal(b, &m) == nil {
			res.Packages = m.Packages
		}
	}
	return res, nil
}

// FileSHA256 streams a file through SHA-256 (sparse-aware enough: reads holes as zeros).
func FileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var urlCredRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s:]+:[^/@\s]+@`)

// Redact removes secrets from log lines (tokens, keys, passwords).
func Redact(s string) string {
	s = urlCredRe.ReplaceAllString(s, "${1}[redacted]@")
	for _, marker := range []string{"token=", "Token:", "password=", "passwordHash", "Authorization:", "PRIVATE KEY", "secret="} {
		if i := strings.Index(s, marker); i >= 0 {
			return s[:i+len(marker)] + " [redacted]"
		}
	}
	if i := strings.Index(s, "$6$"); i >= 0 {
		return s[:i] + "[redacted-hash]"
	}
	return s
}
