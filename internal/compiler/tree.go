package compiler

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File is one entry of a compiled tree. Exactly one of Data, AssetRef or
// ComponentRef is used: assets (uploaded by users) and components (Foundry
// binaries such as the device agent) are resolved by the builder, keeping
// the compiler free of I/O.
type File struct {
	Mode         int64  `json:"mode"`
	Data         []byte `json:"-"`
	AssetRef     string `json:"assetRef,omitempty"`     // "sha256:<hex>"
	ComponentRef string `json:"componentRef,omitempty"` // e.g. "foundry-agent"
	Symlink      string `json:"symlink,omitempty"`
}

// Tree is the compiled mkosi project: relative path -> file.
type Tree struct {
	Files map[string]File
}

func newTree() *Tree { return &Tree{Files: map[string]File{}} }

func (t *Tree) add(p string, mode int64, data string) {
	t.mustClean(p)
	t.Files[p] = File{Mode: mode, Data: []byte(data)}
}

func (t *Tree) addBytes(p string, mode int64, data []byte) {
	t.mustClean(p)
	t.Files[p] = File{Mode: mode, Data: data}
}

func (t *Tree) asset(p, digest string) {
	t.mustClean(p)
	t.Files[p] = File{Mode: 0o644, AssetRef: digest}
}

func (t *Tree) component(p, name string, mode int64) {
	t.mustClean(p)
	t.Files[p] = File{Mode: mode, ComponentRef: name}
}

func (t *Tree) symlink(p, target string) {
	t.mustClean(p)
	t.Files[p] = File{Mode: 0o777, Symlink: target}
}

func (t *Tree) mustClean(p string) {
	if p != path.Clean(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "..") || strings.Contains(p, "/../") {
		panic(fmt.Sprintf("compiler: unclean path %q", p)) // programming error, never user input
	}
}

// Paths returns sorted paths.
func (t *Tree) Paths() []string {
	ps := make([]string, 0, len(t.Files))
	for p := range t.Files {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// Epoch is the fixed mtime of every tar entry (reproducibility).
var Epoch = time.Unix(0, 0).UTC()

// WriteTar writes a deterministic tar (sorted entries, fixed mtime, uid 0).
// Asset and component entries are written as small JSON pointer files under
// the same path with suffix ".ref" so the tar fully describes the tree.
func (t *Tree) WriteTar(w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, p := range t.Paths() {
		f := t.Files[p]
		h := &tar.Header{Name: p, Mode: f.Mode, ModTime: Epoch, Uname: "root", Gname: "root", Format: tar.FormatPAX}
		data := f.Data
		switch {
		case f.Symlink != "":
			h.Typeflag = tar.TypeSymlink
			h.Linkname = f.Symlink
		case f.AssetRef != "":
			h.Name = p + ".ref"
			h.PAXRecords = map[string]string{paxRef: "asset"}
			data = []byte(`{"asset":"` + f.AssetRef + `"}`)
		case f.ComponentRef != "":
			h.Name = p + ".ref"
			h.PAXRecords = map[string]string{paxRef: "component"}
			data = []byte(`{"component":"` + f.ComponentRef + `"}`)
		}
		if h.Typeflag != tar.TypeSymlink {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

const paxRef = "FOUNDRY.ref"

// MaxTreeBytes bounds a compiled tree read back from storage.
const MaxTreeBytes = 64 << 20

// ReadTar parses a tree written by WriteTar (the build VM's input). Entries
// are validated: clean relative paths, regular files or symlinks only.
func ReadTar(r io.Reader) (*Tree, error) {
	t := newTree()
	tr := tar.NewReader(io.LimitReader(r, MaxTreeBytes))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return t, nil
		}
		if err != nil {
			return nil, fmt.Errorf("compiler: read tree: %w", err)
		}
		p := h.Name
		if p != path.Clean(p) || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") {
			return nil, fmt.Errorf("compiler: unsafe path %q in tree", p)
		}
		switch h.Typeflag {
		case tar.TypeSymlink:
			t.Files[p] = File{Mode: 0o777, Symlink: h.Linkname}
		case tar.TypeReg:
			data, err := io.ReadAll(io.LimitReader(tr, MaxTreeBytes))
			if err != nil {
				return nil, err
			}
			if kind := h.PAXRecords[paxRef]; kind != "" {
				var ref struct{ Asset, Component string }
				if err := json.Unmarshal(data, &ref); err != nil {
					return nil, fmt.Errorf("compiler: bad reference %s", p)
				}
				p = strings.TrimSuffix(p, ".ref")
				t.Files[p] = File{Mode: h.Mode, AssetRef: ref.Asset, ComponentRef: ref.Component}
				continue
			}
			t.Files[p] = File{Mode: h.Mode, Data: data}
		default:
			return nil, fmt.Errorf("compiler: unsupported entry type in tree: %s", p)
		}
	}
}

// Digest is sha256 of the deterministic tar.
func (t *Tree) Digest() string {
	h := sha256.New()
	if err := t.WriteTar(h); err != nil {
		panic(err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Resolver supplies bytes for asset and component references.
type Resolver interface {
	Asset(digest string) (io.ReadCloser, error)
	Component(name string) (io.ReadCloser, error)
}

// WriteDir materializes the tree into dir (must be empty or absent).
func (t *Tree) WriteDir(dir string, r Resolver) error {
	for _, p := range t.Paths() {
		f := t.Files[p]
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if f.Symlink != "" {
			if err := os.Symlink(f.Symlink, dst); err != nil {
				return err
			}
			continue
		}
		var src io.ReadCloser
		var err error
		switch {
		case f.AssetRef != "":
			if r == nil {
				return fmt.Errorf("compiler: no resolver for asset %s", f.AssetRef)
			}
			src, err = r.Asset(f.AssetRef)
		case f.ComponentRef != "":
			if r == nil {
				return fmt.Errorf("compiler: no resolver for component %s", f.ComponentRef)
			}
			src, err = r.Component(f.ComponentRef)
		}
		if err != nil {
			return fmt.Errorf("compiler: resolve %s: %w", p, err)
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(f.Mode))
		if err != nil {
			return err
		}
		if src != nil {
			_, err = io.Copy(out, src)
			src.Close()
		} else {
			_, err = out.Write(f.Data)
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := os.Chmod(dst, os.FileMode(f.Mode)); err != nil {
			return err
		}
		if err := os.Chtimes(dst, Epoch, Epoch); err != nil {
			return err
		}
	}
	return nil
}
