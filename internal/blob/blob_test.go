package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPutOpenImmutable(t *testing.T) {
	fs, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data := bytes.Repeat([]byte("foundry"), 300000)
	sum := sha256.Sum256(data)
	want := "sha256:" + hex.EncodeToString(sum[:])
	d, n, err := fs.Put(ctx, bytes.NewReader(data), want, 0)
	if err != nil || d != want || n != int64(len(data)) {
		t.Fatal(err, d, n)
	}
	// idempotent
	if d2, _, err := fs.Put(ctx, bytes.NewReader(data), "", 0); err != nil || d2 != d {
		t.Fatal(err)
	}
	r, size, err := fs.Open(ctx, d)
	if err != nil || size != int64(len(data)) {
		t.Fatal(err)
	}
	r.Seek(7, io.SeekStart)
	b := make([]byte, 7)
	io.ReadFull(r, b)
	r.Close()
	if string(b) != "foundry" {
		t.Fatal(string(b))
	}
	if _, _, err := fs.Put(ctx, strings.NewReader("x"), want, 0); !errors.Is(err, ErrDigestMismatch) {
		t.Fatal("mismatch accepted")
	}
	if _, _, err := fs.Put(ctx, strings.NewReader("12345"), "", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatal("oversize accepted")
	}
	if _, _, err := fs.Open(ctx, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := ParseDigest("sha256:../../etc/passwd"); err == nil {
		t.Fatal("bad digest accepted")
	}
}

func TestSignedURLs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Signer{Key: []byte("k"), BaseURL: "https://api.test", Now: func() time.Time { return now }}
	d := "sha256:" + strings.Repeat("a", 64)
	u, _ := url.Parse(s.DownloadURL(d, "os.qcow2", time.Minute))
	if err := s.VerifyDownload(d, u.Query()); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("name", "evil.sh")
	if s.VerifyDownload(d, q) == nil {
		t.Fatal("tampered name accepted")
	}
	if s.VerifyDownload("sha256:"+strings.Repeat("b", 64), u.Query()) == nil {
		t.Fatal("other digest accepted")
	}
	now = now.Add(2 * time.Minute)
	if s.VerifyDownload(d, u.Query()) == nil {
		t.Fatal("expired accepted")
	}
	now = time.Unix(1_800_000_000, 0)
	pu, _ := url.Parse(s.UploadURL("upl_1", d, 99, time.Minute))
	gotD, size, err := s.VerifyUpload("upl_1", pu.Query())
	if err != nil || gotD != d || size != 99 {
		t.Fatal(err, gotD, size)
	}
	q = pu.Query()
	q.Set("size", "100000000")
	if _, _, err := s.VerifyUpload("upl_1", q); err == nil {
		t.Fatal("size tamper accepted")
	}
}
