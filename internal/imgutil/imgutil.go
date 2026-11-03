// Package imgutil has streaming helpers for image-sized files: sparse copies,
// partition extraction, compression via zstd/xz, and FAT (ESP) edits via
// mtools. Nothing here reads a whole image into memory.
package imgutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

const chunk = 1 << 20

var zeroChunk = make([]byte, chunk)

// SparseCopy copies n bytes from r at off into w at woff, leaving holes for
// all-zero 1 MiB blocks (w must be a fresh, truncated file).
func SparseCopy(w *os.File, woff int64, r io.ReaderAt, off, n int64) error {
	buf := make([]byte, chunk)
	for done := int64(0); done < n; {
		m := min(int64(chunk), n-done)
		if _, err := r.ReadAt(buf[:m], off+done); err != nil && err != io.EOF {
			return err
		}
		if !bytes.Equal(buf[:m], zeroChunk[:m]) {
			if _, err := w.WriteAt(buf[:m], woff+done); err != nil {
				return err
			}
		}
		done += m
	}
	return nil
}

// ExtractRange writes [off, off+n) of src into a new sparse file at dst.
func ExtractRange(src string, off, n int64, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if err := out.Truncate(n); err != nil {
		out.Close()
		return err
	}
	if err := SparseCopy(out, 0, in, off, n); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// CopySparseFile duplicates a (sparse) file.
func CopySparseFile(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	return ExtractRange(src, 0, st.Size(), dst)
}

// WriteAt writes data into a file at an offset.
func WriteAt(path string, off int64, data []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, off); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SHA256File hashes a file while streaming.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, err
}

// Run executes a command, returning combined output on failure.
func Run(ctx context.Context, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		s := out.String()
		if len(s) > 2000 {
			s = s[len(s)-2000:]
		}
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(s))
	}
	return nil
}

// Compression settings.
type Compression struct {
	ZstdLevel int // 19 for releases (docs: zstd -T0 -19 --long=27); lower for dev
	XzLevel   int // 6
}

// DefaultCompression matches docs/context/build-pipeline.md §5.
var DefaultCompression = Compression{ZstdLevel: 19, XzLevel: 6}

// Zstd compresses src to dst.
func Zstd(ctx context.Context, c Compression, src, dst string) error {
	lvl := c.ZstdLevel
	if lvl == 0 {
		lvl = 19
	}
	args := []string{"-q", "-f", "-T0", "-" + strconv.Itoa(lvl), "--long=27", "-o", dst, src}
	if lvl > 19 {
		args = append([]string{"--ultra"}, args...)
	}
	return Run(ctx, nil, "zstd", args...)
}

// Unzstd decompresses to a sparse file.
func Unzstd(ctx context.Context, src, dst string) error {
	return Run(ctx, nil, "zstd", "-q", "-d", "-f", "--long=27", "--sparse", "-o", dst, src)
}

// Xz compresses src to dst (multi-threaded).
func Xz(ctx context.Context, c Compression, src, dst string) error {
	lvl := c.XzLevel
	if lvl == 0 {
		lvl = 6
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "xz", "-T0", "-"+strconv.Itoa(lvl), "-c", src)
	cmd.Stdout = out
	var e bytes.Buffer
	cmd.Stderr = &e
	if err := cmd.Run(); err != nil {
		out.Close()
		return fmt.Errorf("xz: %w: %s", err, e.String())
	}
	return out.Close()
}

// ----------------------------------------------------------------- ESP (FAT)

// FAT edits a FAT file system inside an image at a byte offset using mtools.
type FAT struct {
	Image  string
	Offset int64
	MTime  time.Time // timestamp given to written files (reproducibility)
}

func (f FAT) img() string { return fmt.Sprintf("%s@@%d", f.Image, f.Offset) }

func (f FAT) env() []string { return []string{"MTOOLS_SKIP_CHECK=1", "MTOOLS_NO_VFAT=0", "TZ=UTC"} }

// Mkdir creates a directory and its parents.
func (f FAT) Mkdir(ctx context.Context, dir string) error {
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	for i := range parts {
		p := "::/" + strings.Join(parts[:i+1], "/")
		if f.exists(ctx, p) {
			continue
		}
		if err := Run(ctx, f.env(), "mmd", "-i", f.img(), p); err != nil {
			return err
		}
	}
	return nil
}

func (f FAT) exists(ctx context.Context, p string) bool {
	return exec.CommandContext(ctx, "mdir", "-b", "-i", f.img(), p).Run() == nil
}

// Put copies a local file to dst (absolute path inside the FAT), replacing it.
func (f FAT) Put(ctx context.Context, src, dst string) error {
	if err := f.Mkdir(ctx, path.Dir(dst)); err != nil {
		return err
	}
	if !f.MTime.IsZero() {
		os.Chtimes(src, f.MTime, f.MTime)
	}
	return Run(ctx, f.env(), "mcopy", "-o", "-m", "-i", f.img(), src, "::"+dst)
}

// PutBytes writes data to dst.
func (f FAT) PutBytes(ctx context.Context, data []byte, dst string) error {
	tmp, err := os.CreateTemp("", "fat-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.Write(data)
	tmp.Close()
	return f.Put(ctx, tmp.Name(), dst)
}

// Get reads a file from the FAT.
func (f FAT) Get(ctx context.Context, src string) ([]byte, error) {
	var out, e bytes.Buffer
	cmd := exec.CommandContext(ctx, "mtype", "-i", f.img(), "::"+src)
	cmd.Env = append(os.Environ(), f.env()...)
	cmd.Stdout, cmd.Stderr = &out, &e
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mtype %s: %w: %s", src, err, e.String())
	}
	return out.Bytes(), nil
}

// Exists reports whether a path exists.
func (f FAT) Exists(ctx context.Context, p string) bool { return f.exists(ctx, "::"+p) }
