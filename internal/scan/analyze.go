package scan

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"

	"foundry.dev/foundry/internal/osspec"
)

// PathRule classifies files by path prefix.
type PathRule struct {
	Prefix   string
	Feature  string
	Severity string
	Detail   string
}

// CriticalPaths: writes here are high-risk (security-threat-model.md §6).
var CriticalPaths = []PathRule{
	{"etc/sudoers", "critical-prefix", SevCritical, "sudo configuration"},
	{"etc/ld.so.preload", "critical-prefix", SevCritical, "library preloading"},
	{"root/.ssh/", "critical-prefix", SevCritical, "root SSH keys"},
	{"etc/ssh/sshd_config", "critical-prefix", SevHigh, "SSH daemon configuration"},
	{"etc/pam.d/", "critical-prefix", SevCritical, "authentication stack"},
	{"usr/lib/pam", "critical-prefix", SevCritical, "authentication modules"},
	{"etc/shadow", "critical-prefix", SevCritical, "password database"},
	{"etc/passwd", "critical-prefix", SevHigh, "user database"},
	{"etc/polkit-1/", "critical-prefix", SevHigh, "privilege policy"},
	{"usr/share/polkit-1/", "critical-prefix", SevHigh, "privilege policy"},
	{"etc/systemd/system/", "unit-not-catalog", SevMedium, "adds or enables a systemd unit"},
	{"usr/lib/systemd/system/", "unit-not-catalog", SevMedium, "adds a systemd unit"},
	{"etc/cron", "unit-not-catalog", SevMedium, "scheduled job"},
	{"usr/lib/systemd/boot/", "modified-binary", SevBlocker, "boot loader files are platform-controlled"},
	{"usr/lib/modules/", "modified-binary", SevHigh, "kernel modules"},
	{"etc/apparmor.d/", "security-disabled", SevMedium, "AppArmor profiles"},
}

// pathTrie is a byte trie over CriticalPaths prefixes: lookups cost O(len(path)).
type pathTrie struct {
	kids map[byte]*pathTrie
	rule *PathRule
}

func newPathTrie(rules []PathRule) *pathTrie {
	root := &pathTrie{kids: map[byte]*pathTrie{}}
	for i := range rules {
		n := root
		for j := 0; j < len(rules[i].Prefix); j++ {
			c := rules[i].Prefix[j]
			if n.kids[c] == nil {
				n.kids[c] = &pathTrie{kids: map[byte]*pathTrie{}}
			}
			n = n.kids[c]
		}
		n.rule = &rules[i]
	}
	return root
}

// match returns the longest matching prefix rule.
func (t *pathTrie) match(p string) *PathRule {
	var best *PathRule
	n := t
	for i := 0; i < len(p) && n != nil; i++ {
		n = n.kids[p[i]]
		if n != nil && n.rule != nil {
			best = n.rule
		}
	}
	return best
}

var criticalTrie = newPathTrie(CriticalPaths)

// ClassifyPath returns the rule for a path (relative, no leading slash), if any.
func ClassifyPath(p string) *PathRule {
	return criticalTrie.match(strings.TrimPrefix(path.Clean("/"+p), "/"))
}

// Fetcher opens an uploaded asset by digest.
type Fetcher func(digest string) (io.ReadCloser, error)

// AnalyzeSpec turns expert content into findings: scripts are scanned with
// the content rules, overlays are walked (paths, setuid, ELF, scripts),
// extra repositories and kernel parameters are weighed.
func AnalyzeSpec(s *osspec.Spec, fetch Fetcher) ([]Finding, error) {
	var out []Finding
	if s.Kernel != nil && len(s.Kernel.ExtraCmdline) > 0 {
		out = append(out, Finding{Rule: "SPEC-CMDLINE", Feature: "expert-cmdline", Severity: SevLow, Weight: Weights["expert-cmdline"],
			Detail: "custom kernel parameters: " + strings.Join(s.Kernel.ExtraCmdline, " "), Scanner: "diff"})
	}
	e := s.Expert
	if e == nil {
		return out, nil
	}
	for _, r := range e.ExtraRepos {
		f := Finding{Rule: "SPEC-REPO", Feature: "expert-repo", Severity: SevMedium, Weight: Weights["expert-repo"], Detail: "extra package repository " + r.URL, Scanner: "diff"}
		if !strings.HasPrefix(r.URL, "https://") {
			f.Severity, f.Detail = SevBlocker, "extra repository is not served over HTTPS: "+r.URL
		}
		out = append(out, f)
	}
	for i, d := range e.PostinstScripts {
		name := fmt.Sprintf("expert/postinst-%d", i)
		b, err := readAsset(fetch, d, 1<<20)
		if err != nil {
			return nil, err
		}
		out = append(out, Finding{Rule: "SPEC-SCRIPT", Feature: "expert-script", Severity: SevLow, Weight: Weights["expert-script"], Path: name, Detail: "custom build script", Scanner: "script"})
		out = append(out, ScanText(name, b)...)
	}
	for i, d := range e.Overlays {
		f, err := analyzeOverlay(fmt.Sprintf("expert/overlay-%d", i), d, fetch)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	for i := range e.MkosiDropins {
		out = append(out, Finding{Rule: "SPEC-MKOSI", Feature: "expert-script", Severity: SevMedium, Weight: 15, Path: fmt.Sprintf("expert/mkosi-%d.conf", i),
			Detail: "raw mkosi configuration", Scanner: "diff"})
	}
	return out, nil
}

func readAsset(fetch Fetcher, d string, max int64) ([]byte, error) {
	rc, err := fetch(d)
	if err != nil {
		return nil, fmt.Errorf("scan: asset %s: %w", d, err)
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, max))
}

func analyzeOverlay(name, digest string, fetch Fetcher) ([]Finding, error) {
	rc, err := fetch(digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	br := bufio.NewReader(rc)
	var src io.Reader = br
	if m, _ := br.Peek(2); len(m) == 2 && m[0] == 0x1f && m[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		src = gz
	}
	tr := tar.NewReader(src)
	var out []Finding
	elf := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		p := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		if r := ClassifyPath(p); r != nil {
			out = append(out, Finding{Rule: "DIFF-PATH", Feature: r.Feature, Severity: r.Severity, Weight: Weights[r.Feature], Path: name + ":/" + p, Detail: r.Detail, Scanner: "diff"})
		}
		if h.Mode&0o6000 != 0 && h.Typeflag == tar.TypeReg {
			out = append(out, Finding{Rule: "DIFF-SUID", Feature: "setuid", Severity: SevHigh, Weight: Weights["setuid"], Path: name + ":/" + p, Detail: "setuid/setgid file", Scanner: "diff"})
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		head := make([]byte, 4)
		n, _ := io.ReadFull(tr, head)
		head = head[:n]
		if bytes.Equal(head, []byte("\x7fELF")) {
			elf++
			continue
		}
		if h.Size <= 1<<20 {
			rest, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
			out = append(out, ScanText(name+":/"+p, append(head, rest...))...)
		}
	}
	if elf > 0 {
		out = append(out, Finding{Rule: "DIFF-ELF", Feature: "uploaded-elf", Severity: SevMedium, Weight: Weights["uploaded-elf"], Path: name,
			Detail: fmt.Sprintf("%d uploaded executable(s)", elf), Scanner: "diff"})
	}
	return out, nil
}
