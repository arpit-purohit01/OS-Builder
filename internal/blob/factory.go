package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Presigner is implemented by stores that can hand clients a direct,
// time-limited download URL (the API then redirects instead of streaming).
type Presigner interface {
	PresignGET(digest, filename string, ttl time.Duration) (string, error)
}

// FromEnv builds the artifact store from the environment:
//
//	FOUNDRY_S3_BUCKET (+ FOUNDRY_S3_ENDPOINT, _REGION, _ACCESS_KEY, _SECRET_KEY, _SESSION_TOKEN,
//	_PREFIX, _PATH_STYLE=0|1, _PART_MIB, _CONCURRENCY) selects the S3 backend;
//	otherwise objects live on the filesystem under FOUNDRY_BLOBS (default fsDefault).
//
// get is os.Getenv (a parameter so it can be tested).
func FromEnv(get func(string) string, fsDefault string) (Store, error) {
	bucket := get("FOUNDRY_S3_BUCKET")
	if bucket == "" {
		dir := get("FOUNDRY_BLOBS")
		if dir == "" {
			dir = fsDefault
		}
		return NewFS(dir)
	}
	s := &S3{
		Endpoint: get("FOUNDRY_S3_ENDPOINT"), Region: get("FOUNDRY_S3_REGION"), Bucket: bucket,
		Prefix: get("FOUNDRY_S3_PREFIX"),
		Creds:  Credentials{AccessKey: get("FOUNDRY_S3_ACCESS_KEY"), SecretKey: get("FOUNDRY_S3_SECRET_KEY"), SessionToken: get("FOUNDRY_S3_SESSION_TOKEN")},
	}
	if s.Prefix != "" && !strings.HasSuffix(s.Prefix, "/") {
		s.Prefix += "/"
	}
	if s.Endpoint == "" {
		r := s.Region
		if r == "" {
			r = "us-east-1"
		}
		s.Endpoint = "https://s3." + r + ".amazonaws.com"
		s.PathStyle = false
	} else {
		s.PathStyle = get("FOUNDRY_S3_PATH_STYLE") != "0" // MinIO and friends need path style
	}
	if v := get("FOUNDRY_S3_PART_MIB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 {
			return nil, fmt.Errorf("FOUNDRY_S3_PART_MIB must be at least 5")
		}
		s.PartSize = int64(n) << 20
	}
	if v := get("FOUNDRY_S3_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 32 {
			return nil, fmt.Errorf("FOUNDRY_S3_CONCURRENCY must be 1..32")
		}
		s.Concurrency = n
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Local returns a filesystem path holding the object: the object itself for
// the filesystem backend, otherwise a temporary copy streamed into dir. The
// caller must call cleanup when done (it is always non-nil).
func Local(ctx context.Context, s Store, digest, dir string) (path string, cleanup func(), err error) {
	if p, ok := s.Path(digest); ok {
		return p, func() {}, nil
	}
	rc, _, err := s.Open(ctx, digest)
	if err != nil {
		return "", func() {}, err
	}
	defer rc.Close()
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "blob-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { os.Remove(f.Name()) }
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), ctxReader{ctx, rc}); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		cleanup()
		return "", func() {}, fmt.Errorf("%w: stored object hashes to %s", ErrDigestMismatch, got)
	}
	return f.Name(), cleanup, nil
}
