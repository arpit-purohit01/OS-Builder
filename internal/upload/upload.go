// Package upload validates user-supplied files before they can be referenced
// by a spec (P2-08): images, fonts, and overlay archives. Archives are parsed
// as a stream and never extracted on the platform.
package upload

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits for archive validation.
type Limits struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxEntries    int
	MaxRatio      float64 // uncompressed / compressed
	AllowSetuid   bool    // expert + review only
}

// DefaultLimits per docs/plan/phase-2 P2-08.
var DefaultLimits = Limits{MaxFileBytes: 100 << 20, MaxTotalBytes: 2 << 30, MaxEntries: 200000, MaxRatio: 100}

// Finding is one problem in an upload.
type Finding struct {
	Path   string `json:"path,omitempty"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

// Report summarizes validation.
type Report struct {
	Valid    bool      `json:"valid"`
	Kind     string    `json:"kind"`
	Entries  int       `json:"entries,omitempty"`
	Bytes    int64     `json:"bytes,omitempty"`
	Width    int       `json:"width,omitempty"`
	Height   int       `json:"height,omitempty"`
	Format   string    `json:"format,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	Setuid   []string  `json:"setuid,omitempty"`
	Scripts  []string  `json:"scripts,omitempty"`
	ELF      []string  `json:"elf,omitempty"`
	// Package is a .deb's package name; Fingerprint an OpenPGP key's.
	Package     string `json:"package,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

func (r *Report) add(p, rule, detail string) {
	r.Findings = append(r.Findings, Finding{Path: p, Rule: rule, Detail: detail})
}

// ValidateImage checks a PNG/JPEG (decoded header) or SVG (no script/external refs).
func ValidateImage(data []byte, maxPixels int) *Report {
	rep := &Report{Kind: "image"}
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		rep.Width, rep.Height, rep.Format = cfg.Width, cfg.Height, format
		if cfg.Width*cfg.Height > maxPixels || cfg.Width <= 0 || cfg.Height <= 0 {
			rep.add("", "UPL-IMG-001", fmt.Sprintf("image is %dx%d; maximum is %d pixels", cfg.Width, cfg.Height, maxPixels))
		}
		// Fully decode to catch truncated/malformed files (decompression bombs are bounded by maxPixels).
		if len(rep.Findings) == 0 {
			if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
				rep.add("", "UPL-IMG-002", "image data is corrupt: "+err.Error())
			}
		}
	} else if isSVG(data) {
		rep.Format = "svg"
		lower := strings.ToLower(string(data))
		for _, bad := range []string{"<script", "javascript:", "onload=", "onerror=", "<foreignobject", "xlink:href=\"http", "href=\"http", "<!entity", "<iframe"} {
			if strings.Contains(lower, bad) {
				rep.add("", "UPL-SVG-001", "SVG contains active or external content ("+bad+")")
			}
		}
	} else {
		rep.add("", "UPL-IMG-000", "not a PNG, JPEG or SVG image")
	}
	rep.Valid = len(rep.Findings) == 0
	return rep
}

func isSVG(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 1024)]))
	return strings.Contains(head, "<svg") && utf8.Valid(b)
}

// countingReader counts compressed bytes consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ValidateArchive checks a tar (optionally gzip) overlay tree.
func ValidateArchive(r io.Reader, lim Limits) (*Report, error) {
	rep := &Report{Kind: "overlay"}
	cr := &countingReader{r: r}
	br := bufio.NewReader(cr)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		src = gz
	}
	tr := tar.NewReader(src)
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rep.add("", "UPL-TAR-000", "archive is corrupt: "+err.Error())
			break
		}
		rep.Entries++
		if rep.Entries > lim.MaxEntries {
			rep.add("", "UPL-TAR-001", fmt.Sprintf("more than %d entries", lim.MaxEntries))
			break
		}
		name, reason := cleanName(h.Name)
		if reason != "" {
			rep.add(h.Name, "UPL-PATH", reason)
			continue
		}
		if seen[name] {
			rep.add(name, "UPL-DUP", "duplicate entry")
		}
		seen[name] = true
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		case tar.TypeSymlink:
			if t := h.Linkname; strings.HasPrefix(t, "/") || escapes(path.Dir(name), t) {
				rep.add(name, "UPL-LINK", "symlink points outside the overlay ("+t+")")
			}
		case tar.TypeLink:
			rep.add(name, "UPL-HARDLINK", "hard links are not allowed")
		default:
			rep.add(name, "UPL-TYPE", "device nodes, FIFOs and sockets are not allowed")
		}
		if h.Mode&04000 != 0 || h.Mode&02000 != 0 {
			rep.Setuid = append(rep.Setuid, name)
			if !lim.AllowSetuid {
				rep.add(name, "UPL-SUID", "setuid/setgid files need expert mode and review")
			}
		}
		if h.Typeflag == tar.TypeReg {
			if h.Size > lim.MaxFileBytes {
				rep.add(name, "UPL-SIZE", fmt.Sprintf("file larger than %d bytes", lim.MaxFileBytes))
			}
			var head [4]byte
			n, _ := io.ReadFull(tr, head[:])
			rest, err := io.Copy(io.Discard, io.LimitReader(tr, lim.MaxFileBytes+1))
			if err != nil {
				rep.add(name, "UPL-TAR-000", err.Error())
				break
			}
			sz := int64(n) + rest
			total += sz
			if string(head[:n]) == "\x7fELF" {
				rep.ELF = append(rep.ELF, name)
			} else if n >= 2 && head[0] == '#' && head[1] == '!' {
				rep.Scripts = append(rep.Scripts, name)
			}
			if total > lim.MaxTotalBytes {
				rep.add("", "UPL-TOTAL", "archive expands beyond the total size limit")
				break
			}
			if cr.n > 0 && float64(total)/float64(cr.n) > lim.MaxRatio && total > 10<<20 {
				rep.add("", "UPL-RATIO", "compression ratio too high (possible archive bomb)")
				break
			}
		}
	}
	rep.Bytes = total
	rep.Valid = len(rep.Findings) == 0
	return rep, nil
}

// cleanName normalizes a member name and rejects unsafe ones.
func cleanName(n string) (string, string) {
	if n == "" {
		return "", "empty name"
	}
	if !utf8.ValidString(n) {
		return "", "name is not valid UTF-8"
	}
	for _, r := range n {
		if r == 0 || unicode.IsControl(r) {
			return "", "name contains control characters"
		}
	}
	if strings.HasPrefix(n, "/") {
		return "", "absolute path"
	}
	n = strings.TrimPrefix(n, "./")
	for _, seg := range strings.Split(strings.TrimSuffix(n, "/"), "/") {
		if seg == ".." {
			return "", "path contains .."
		}
		if len(seg) > 255 {
			return "", "path component longer than 255 bytes"
		}
	}
	if len(n) > 4096 {
		return "", "path longer than 4096 bytes"
	}
	n = norm.NFC.String(path.Clean(n))
	return n, ""
}

// escapes reports whether a relative symlink target leaves the root.
func escapes(dir, target string) bool {
	return strings.HasPrefix(path.Clean(path.Join("/", dir, target)), "/..") ||
		path.Clean(path.Join(dir, target)) == ".." || strings.HasPrefix(path.Clean(path.Join(dir, target)), "../")
}

// ErrUnsupported is returned for unknown upload kinds.
var ErrUnsupported = errors.New("unsupported upload kind")
