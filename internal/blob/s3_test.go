package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The "get-vanilla" vector of the AWS Signature Version 4 test suite.
func TestSigV4Vector(t *testing.T) {
	c := Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	ts := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	canon, signed := canonicalRequest("GET", "/", url.Values{}, map[string]string{"host": "example.amazonaws.com", "x-amz-date": "20150830T123600Z"}, emptySHA256)
	if signed != "host;x-amz-date" {
		t.Fatal(signed)
	}
	scope := "20150830/us-east-1/service/aws4_request"
	if got := hexSHA256([]byte(canon)); got != "bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63" {
		t.Fatalf("canonical request hash %s", got)
	}
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, "20150830", "us-east-1", "service"), stringToSign(ts, scope, canon)))
	if sig != "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31" {
		t.Fatalf("signature %s", sig)
	}
}

// fakeS3 is an in-memory S3 that verifies SigV4 on every request.
type fakeS3 struct {
	t       *testing.T
	creds   Credentials
	bucket  string
	mu      sync.Mutex
	objs    map[string][]byte
	uploads map[string]map[int][]byte
	log     []string
	flaky   int // fail the next N part uploads with 503
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !f.verify(r, body) {
		http.Error(w, `<Error><Code>SignatureDoesNotMatch</Code><Message>bad signature</Message></Error>`, 403)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/"+f.bucket+"/")
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, r.Method+" "+key+"?"+r.URL.RawQuery)
	switch {
	case r.Method == "POST" && q.Has("uploads"):
		id := fmt.Sprintf("up%d", len(f.uploads)+1)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == "PUT" && q.Has("partNumber"):
		if f.flaky > 0 {
			f.flaky--
			http.Error(w, `<Error><Code>InternalError</Code><Message>try again</Message></Error>`, 503)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		up := f.uploads[q.Get("uploadId")]
		if up == nil {
			http.Error(w, `<Error><Code>NoSuchUpload</Code></Error>`, 404)
			return
		}
		data := body
		if src := r.Header.Get("x-amz-copy-source"); src != "" {
			var a, b int
			fmt.Sscanf(r.Header.Get("x-amz-copy-source-range"), "bytes=%d-%d", &a, &b)
			data = f.objs[strings.TrimPrefix(mustUnescape(src), "/"+f.bucket+"/")][a : b+1]
		}
		up[n] = append([]byte(nil), data...)
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
		fmt.Fprint(w, "<CopyPartResult><ETag>etag</ETag></CopyPartResult>")
	case r.Method == "POST" && q.Has("uploadId"):
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			http.Error(w, `<Error><Code>MalformedXML</Code></Error>`, 400)
			return
		}
		up := f.uploads[q.Get("uploadId")]
		var all []byte
		last := 0
		for _, p := range req.Parts {
			if p.PartNumber <= last || up[p.PartNumber] == nil {
				http.Error(w, `<Error><Code>InvalidPart</Code></Error>`, 400)
				return
			}
			last = p.PartNumber
			all = append(all, up[p.PartNumber]...)
		}
		delete(f.uploads, q.Get("uploadId"))
		f.objs[key] = all
		fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"x"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == "PUT" && r.Header.Get("x-amz-copy-source") != "":
		src := strings.TrimPrefix(mustUnescape(r.Header.Get("x-amz-copy-source")), "/"+f.bucket+"/")
		d, ok := f.objs[src]
		if !ok {
			http.Error(w, `<Error><Code>NoSuchKey</Code></Error>`, 404)
			return
		}
		f.objs[key] = d
		fmt.Fprint(w, "<CopyObjectResult><ETag>x</ETag></CopyObjectResult>")
	case r.Method == "PUT":
		f.objs[key] = body
	case r.Method == "DELETE" && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(204)
	case r.Method == "DELETE":
		delete(f.objs, key)
		w.WriteHeader(204)
	case r.Method == "HEAD" || r.Method == "GET":
		d, ok := f.objs[key]
		if !ok {
			if r.Method == "HEAD" {
				w.WriteHeader(404)
			} else {
				http.Error(w, `<Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`, 404)
			}
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(d))
	default:
		http.Error(w, "unsupported", 400)
	}
}

func mustUnescape(s string) string { u, _ := url.PathUnescape(s); return u }

func (f *fakeS3) verify(r *http.Request, body []byte) bool {
	q := r.URL.Query()
	if q.Get("X-Amz-Signature") != "" { // presigned
		t, err := time.Parse(amzDate, q.Get("X-Amz-Date"))
		if err != nil {
			return false
		}
		exp, _ := strconv.Atoi(q.Get("X-Amz-Expires"))
		if time.Now().After(t.Add(time.Duration(exp) * time.Second)) {
			return false
		}
		got := q.Get("X-Amz-Signature")
		q.Del("X-Amz-Signature")
		u := *r.URL
		u.Host = r.Host
		u.RawQuery = q.Encode()
		want := presign(r.Method, &u, stripPresign(q), f.creds, "us-east-1", "s3", t, time.Duration(exp)*time.Second)
		return strings.HasSuffix(want, "X-Amz-Signature="+got)
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return false
	}
	t, err := time.Parse(amzDate, r.Header.Get("x-amz-date"))
	if err != nil || time.Since(t) > 5*time.Minute {
		return false
	}
	if h := r.Header.Get("x-amz-content-sha256"); h != hexSHA256(body) {
		return false
	}
	clone := r.Clone(r.Context())
	clone.URL.Host = r.Host
	clone.Header.Del("Authorization")
	clone.Header.Del("x-amz-date")
	clone.Header.Del("x-amz-content-sha256")
	signRequest(clone, f.creds, "us-east-1", "s3", hexSHA256(body), t)
	return clone.Header.Get("Authorization") == auth
}

func stripPresign(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		if !strings.HasPrefix(k, "X-Amz-") {
			out[k] = v
		}
	}
	return out
}

func newFake(t *testing.T) (*S3, *fakeS3) {
	f := &fakeS3{t: t, creds: Credentials{AccessKey: "AKIAFOUNDRY", SecretKey: "s3cr3t/key+value"}, bucket: "artifacts", objs: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &S3{Endpoint: srv.URL, Bucket: "artifacts", Creds: f.creds, PathStyle: true, PartSize: 5 << 20, Concurrency: 4}, f
}

func digestOf(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

func TestS3PutGet(t *testing.T) {
	s, f := newFake(t)
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))
	for _, size := range []int{0, 1, 5<<20 - 1, 5 << 20, 5<<20 + 1, 23<<20 + 12345} {
		data := make([]byte, size)
		rng.Read(data)
		d, n, err := s.Put(ctx, bytes.NewReader(data), "", 0)
		if err != nil || n != int64(size) || d != digestOf(data) {
			t.Fatalf("size %d: %v %d %s", size, err, n, d)
		}
		sz, err := s.Stat(ctx, d)
		if err != nil || sz != int64(size) {
			t.Fatalf("stat %d: %v %d", size, err, sz)
		}
		rc, sz, err := s.Open(ctx, d)
		if err != nil || sz != int64(size) {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: read back mismatch (%v)", size, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.objs {
		if strings.HasPrefix(k, "tmp/") {
			t.Errorf("temporary object left behind: %s", k)
		}
	}
	if len(f.uploads) != 0 {
		t.Errorf("unfinished uploads: %d", len(f.uploads))
	}
}

func TestS3Seek(t *testing.T) {
	s, _ := newFake(t)
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(2)).Read(data)
	d, _, err := s.Put(context.Background(), bytes.NewReader(data), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	rc.Seek(-100, io.SeekEnd)
	b := make([]byte, 100)
	if _, err := io.ReadFull(rc, b); err != nil || !bytes.Equal(b, data[len(data)-100:]) {
		t.Fatal("tail", err)
	}
	rc.Seek(1000, io.SeekStart)
	io.ReadFull(rc, b)
	if !bytes.Equal(b, data[1000:1100]) {
		t.Fatal("middle")
	}
	// http.ServeContent works over it (what the API does for downloads)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Range", "bytes=10-19")
	http.ServeContent(rec, req, "", time.Time{}, rc)
	if rec.Code != 206 || !bytes.Equal(rec.Body.Bytes(), data[10:20]) {
		t.Fatalf("range: %d", rec.Code)
	}
}

func TestS3DigestMismatchAndLimits(t *testing.T) {
	s, f := newFake(t)
	data := bytes.Repeat([]byte("x"), 12<<20)
	_, _, err := s.Put(context.Background(), bytes.NewReader(data), "sha256:"+strings.Repeat("0", 64), 0)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	if _, _, err := s.Put(context.Background(), bytes.NewReader(data), "", 7<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want too large, got %v", err)
	}
	small := []byte("hello")
	if _, _, err := s.Put(context.Background(), bytes.NewReader(small), "sha256:"+strings.Repeat("1", 64), 0); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("small mismatch: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.objs) != 0 || len(f.uploads) != 0 {
		t.Fatalf("failed uploads left state: %d objects, %d uploads", len(f.objs), len(f.uploads))
	}
}

func TestS3RetriesAndAbort(t *testing.T) {
	s, f := newFake(t)
	s.Retries = 3
	data := make([]byte, 11<<20)
	rand.New(rand.NewSource(3)).Read(data)
	f.flaky = 2
	if _, _, err := s.Put(context.Background(), bytes.NewReader(data), digestOf(data), 0); err != nil {
		t.Fatalf("transient failures should be retried: %v", err)
	}
	f.flaky = 1000
	if _, _, err := s.Put(context.Background(), bytes.NewReader(append(data, 1)), "", 0); err == nil {
		t.Fatal("persistent failure should surface")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) != 0 {
		t.Fatalf("multipart upload not aborted: %d", len(f.uploads))
	}
}

func TestS3NotFoundAndDelete(t *testing.T) {
	s, _ := newFake(t)
	ctx := context.Background()
	d := "sha256:" + strings.Repeat("ab", 32)
	if _, err := s.Stat(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat: %v", err)
	}
	if _, _, err := s.Open(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open: %v", err)
	}
	dd, _, _ := s.Put(ctx, strings.NewReader("data"), "", 0)
	if err := s.Delete(ctx, dd); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, dd); err != nil {
		t.Fatal("delete must be idempotent:", err)
	}
	if _, err := s.Stat(ctx, dd); !errors.Is(err, ErrNotFound) {
		t.Fatal("still there")
	}
}

func TestS3BadCredentialsAndPresign(t *testing.T) {
	s, _ := newFake(t)
	ctx := context.Background()
	d, _, err := s.Put(ctx, strings.NewReader("payload"), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.PresignGET(d, "my image (1).raw", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "payload" {
		t.Fatalf("presigned GET: %d %q", resp.StatusCode, b)
	}
	// tampering with any signed part breaks it
	if r2, _ := http.Get(strings.Replace(u, "X-Amz-Expires=300", "X-Amz-Expires=900", 1)); r2 != nil && r2.StatusCode == 200 {
		t.Fatal("expiry tampering accepted")
	}
	bad := *s
	bad.Creds.SecretKey = "wrong"
	if _, err := bad.Stat(ctx, d); err == nil {
		t.Fatal("bad credentials accepted")
	}
	if _, _, err := bad.Put(ctx, strings.NewReader("x"), "", 0); err == nil {
		t.Fatal("bad credentials accepted for put")
	}
}

func TestS3LargeCopyPath(t *testing.T) {
	s, f := newFake(t)
	// Force the ranged-copy path with small numbers by copying a 3-part object manually.
	data := make([]byte, 7<<20)
	rand.New(rand.NewSource(4)).Read(data)
	f.mu.Lock()
	f.objs["tmp/src"] = data
	f.mu.Unlock()
	defer func(l, r int64) { copyLimit, copyRange = l, r }(copyLimit, copyRange)
	copyLimit, copyRange = 1<<20, 3<<20 // 7 MiB → ranges of 3 MiB
	if err := s.copyObject(context.Background(), "tmp/src", "sha256/dst", int64(len(data))); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.objs["sha256/dst"], data) {
		t.Fatal("copy mismatch")
	}
}

func TestS3Validate(t *testing.T) {
	for _, s := range []*S3{{}, {Endpoint: "ftp://x", Bucket: "b", Creds: Credentials{"a", "b", ""}}, {Endpoint: "http://x", Bucket: "b", Prefix: "../x", Creds: Credentials{"a", "b", ""}}} {
		if s.Validate() == nil {
			t.Errorf("accepted %+v", s)
		}
	}
}

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	st, err := FromEnv(get(map[string]string{"FOUNDRY_BLOBS": dir}), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*FS); !ok {
		t.Fatalf("%T", st)
	}
	st, err = FromEnv(get(map[string]string{"FOUNDRY_S3_BUCKET": "b", "FOUNDRY_S3_ENDPOINT": "http://127.0.0.1:9", "FOUNDRY_S3_ACCESS_KEY": "a", "FOUNDRY_S3_SECRET_KEY": "s"}), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s3, ok := st.(*S3); !ok || !s3.PathStyle {
		t.Fatalf("%T", st)
	}
	if _, err = FromEnv(get(map[string]string{"FOUNDRY_S3_BUCKET": "b"}), dir); err == nil {
		t.Fatal("incomplete S3 config accepted")
	}
	_ = os.Getenv
	_ = sort.Strings
}
