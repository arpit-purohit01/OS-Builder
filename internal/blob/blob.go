// Package blob is the content-addressed artifact store (ADR-0009): objects
// are named sha256/<hex>, written once, never mutated. Downloads and uploads
// use short-lived HMAC-signed URLs so artifacts never stream through API
// memory.
package blob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound       = errors.New("blob not found")
	ErrDigestMismatch = errors.New("blob digest mismatch")
	ErrTooLarge       = errors.New("blob exceeds size limit")
)

// Store is implemented by the filesystem backend (and S3-compatible backends).
type Store interface {
	Put(ctx context.Context, r io.Reader, expectDigest string, maxSize int64) (digest string, size int64, err error)
	Open(ctx context.Context, digest string) (ReadSeekCloser, int64, error)
	Stat(ctx context.Context, digest string) (int64, error)
	Delete(ctx context.Context, digest string) error
	Path(digest string) (string, bool) // local path when the backend is a filesystem
}

type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

// FS stores objects under root/sha256/<2>/<hex>.
type FS struct {
	Root string
	mu   sync.Mutex
}

// NewFS creates the directory layout.
func NewFS(root string) (*FS, error) {
	for _, d := range []string{"sha256", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			return nil, err
		}
	}
	return &FS{Root: root}, nil
}

// ParseDigest validates "sha256:<64 hex>" and returns the hex.
func ParseDigest(d string) (string, error) {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) != 64 || h != strings.ToLower(h) {
		return "", fmt.Errorf("invalid digest %q", d)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("invalid digest %q", d)
	}
	return h, nil
}

func (f *FS) objPath(h string) string { return filepath.Join(f.Root, "sha256", h[:2], h) }

func (f *FS) Path(digest string) (string, bool) {
	h, err := ParseDigest(digest)
	if err != nil {
		return "", false
	}
	p := f.objPath(h)
	_, err = os.Stat(p)
	return p, err == nil
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// Put streams r to a temp file while hashing, verifies the digest if given,
// then atomically renames into place (idempotent for existing objects).
func (f *FS) Put(ctx context.Context, r io.Reader, expect string, maxSize int64) (string, int64, error) {
	tmp, err := os.CreateTemp(filepath.Join(f.Root, "tmp"), "put-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	var src io.Reader = r
	if maxSize > 0 {
		src = io.LimitReader(r, maxSize+1)
	}
	bp := bufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(io.MultiWriter(tmp, h), ctxReader{ctx, src}, *bp)
	bufPool.Put(bp)
	if err != nil {
		tmp.Close()
		return "", 0, err
	}
	if maxSize > 0 && n > maxSize {
		tmp.Close()
		return "", 0, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	hx := hex.EncodeToString(h.Sum(nil))
	digest := "sha256:" + hx
	if expect != "" && expect != digest {
		return "", 0, fmt.Errorf("%w: expected %s got %s", ErrDigestMismatch, expect, digest)
	}
	dst := f.objPath(hx)
	if _, err := os.Stat(dst); err == nil {
		return digest, n, nil // immutable: already present
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", 0, err
	}
	if err := os.Chmod(tmp.Name(), 0o440); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, err
	}
	return digest, n, nil
}

// PutFile ingests a local file (streaming, hashed while copying).
func (f *FS) PutFile(ctx context.Context, path string) (string, int64, error) {
	return PutFile(ctx, f, path)
}

// PutFile ingests a local file into any store.
func PutFile(ctx context.Context, s Store, path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	return s.Put(ctx, file, "", 0)
}

func (f *FS) Open(ctx context.Context, digest string) (ReadSeekCloser, int64, error) {
	h, err := ParseDigest(digest)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(f.objPath(h))
	if os.IsNotExist(err) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, st.Size(), nil
}

func (f *FS) Stat(ctx context.Context, digest string) (int64, error) {
	h, err := ParseDigest(digest)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(f.objPath(h))
	if os.IsNotExist(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Delete is used only by retention GC.
func (f *FS) Delete(ctx context.Context, digest string) error {
	h, err := ParseDigest(digest)
	if err != nil {
		return err
	}
	err = os.Remove(f.objPath(h))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ------------------------------------------------------------ signed URLs

// Signer creates and verifies HMAC-signed object URLs.
type Signer struct {
	Key     []byte
	BaseURL string // e.g. https://api.foundry.example
	Now     func() time.Time
}

func (s *Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Signer) mac(parts ...string) string {
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

// DownloadURL returns a GET URL valid for ttl, optionally with a filename.
func (s *Signer) DownloadURL(digest, name string, ttl time.Duration) string {
	exp := strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	v := url.Values{"exp": {exp}, "sig": {s.mac("GET", digest, exp, name)}}
	if name != "" {
		v.Set("name", name)
	}
	return s.BaseURL + "/v1/blobs/" + digest + "?" + v.Encode()
}

// VerifyDownload checks a GET URL's signature.
func (s *Signer) VerifyDownload(digest string, q url.Values) error {
	return s.verify(q, "GET", digest, q.Get("exp"), q.Get("name"))
}

// UploadURL returns a PUT URL bound to an upload id, expected digest and max size.
func (s *Signer) UploadURL(uploadID, digest string, size int64, ttl time.Duration) string {
	exp := strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	sz := strconv.FormatInt(size, 10)
	v := url.Values{"exp": {exp}, "digest": {digest}, "size": {sz}, "sig": {s.mac("PUT", uploadID, digest, sz, exp)}}
	return s.BaseURL + "/v1/uploads/" + uploadID + "?" + v.Encode()
}

// VerifyUpload checks a PUT URL and returns the expected digest and size.
func (s *Signer) VerifyUpload(uploadID string, q url.Values) (string, int64, error) {
	if err := s.verify(q, "PUT", uploadID, q.Get("digest"), q.Get("size"), q.Get("exp")); err != nil {
		return "", 0, err
	}
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if err != nil {
		return "", 0, err
	}
	return q.Get("digest"), size, nil
}

func (s *Signer) verify(q url.Values, parts ...string) error {
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		return errors.New("signature: bad expiry")
	}
	if s.now().Unix() > exp {
		return errors.New("signature: expired")
	}
	want := s.mac(parts...)
	if !hmac.Equal([]byte(want), []byte(q.Get("sig"))) {
		return errors.New("signature: invalid")
	}
	return nil
}
