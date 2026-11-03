package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// S3 stores objects in an S3-compatible bucket (AWS S3, MinIO, Ceph RGW, …)
// under <prefix>sha256/<hex>. Uploads stream: the SHA-256 is computed while
// reading, parts of PartSize are uploaded by up to Concurrency workers, and
// the finished object is copied to its content-addressed key — so a wrong or
// truncated upload never occupies an immutable name.
type S3 struct {
	Endpoint    string // https://s3.eu-central-1.amazonaws.com, http://127.0.0.1:9000, …
	Region      string
	Bucket      string
	Prefix      string // optional key prefix, e.g. "prod/"
	Creds       Credentials
	PathStyle   bool         // bucket in the path (MinIO, Ceph); virtual-hosted otherwise
	PartSize    int64        // default 64 MiB
	Concurrency int          // default 4
	Retries     int          // default 4
	HTTP        *http.Client // default: 10 min timeout per request
	Now         func() time.Time
}

const (
	defaultPartSize = 64 << 20
	minPartSize     = 5 << 20
	maxParts        = 10000
)

// CopyObject handles up to 5 GiB; larger objects are copied in ranges.
var (
	copyLimit int64 = 4 << 30
	copyRange int64 = 1 << 30
)

var prefixRe = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)

// Validate checks the configuration.
func (s *S3) Validate() error {
	if s.Endpoint == "" || s.Bucket == "" || s.Creds.AccessKey == "" || s.Creds.SecretKey == "" {
		return errors.New("s3: endpoint, bucket and credentials are required")
	}
	if u, err := url.Parse(s.Endpoint); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("s3: bad endpoint %q", s.Endpoint)
	}
	if !prefixRe.MatchString(s.Prefix) || strings.Contains(s.Prefix, "..") {
		return errors.New("s3: bad key prefix")
	}
	return nil
}

func (s *S3) region() string {
	if s.Region == "" {
		return "us-east-1"
	}
	return s.Region
}

func (s *S3) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *S3) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s *S3) partSize() int64 {
	if s.PartSize >= minPartSize {
		return s.PartSize
	}
	return defaultPartSize
}

func (s *S3) conc() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return 4
}

func (s *S3) retries() int {
	if s.Retries > 0 {
		return s.Retries
	}
	return 4
}

func (s *S3) key(digestHex string) string { return s.Prefix + "sha256/" + digestHex }

// objectURL builds the (canonically encoded) URL of a key.
func (s *S3) objectURL(key string, q url.Values) *url.URL {
	u, _ := url.Parse(s.Endpoint)
	host, path := u.Host, "/"+key
	if s.PathStyle {
		path = "/" + s.Bucket + path
	} else {
		host = s.Bucket + "." + host
	}
	out := &url.URL{Scheme: u.Scheme, Host: host, Path: path}
	out.RawPath = awsEncode(path, false)
	if q != nil {
		out.RawQuery = canonicalQuery(q)
	}
	return out
}

// S3Error is an error response from the service.
type S3Error struct {
	Status  int
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	return fmt.Sprintf("s3: %s (%d): %s", e.Code, e.Status, e.Message)
}

func (e *S3Error) retryable() bool {
	return e.Status >= 500 || e.Status == 429 || e.Code == "RequestTimeout" || e.Code == "SlowDown"
}

func parseS3Error(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &S3Error{Status: resp.StatusCode}
	var x struct{ Code, Message string }
	if xml.Unmarshal(b, &x) == nil && x.Code != "" {
		e.Code, e.Message = x.Code, x.Message
	} else {
		e.Code, e.Message = http.StatusText(resp.StatusCode), strings.TrimSpace(string(b))
	}
	return e
}

// do sends one signed request with retries on transient failures. body must
// be replayable (bytes) because the payload hash is signed.
func (s *S3) do(ctx context.Context, method string, u *url.URL, hdr map[string]string, body []byte, ok ...int) (*http.Response, error) {
	var last error
	for attempt := 0; attempt <= s.retries(); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 200 * time.Millisecond):
			}
		}
		var rd io.Reader
		hash := emptySHA256
		if body != nil {
			rd = bytes.NewReader(body)
			hash = hexSHA256(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.ContentLength = int64(len(body))
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		signRequest(req, s.Creds, s.region(), "s3", hash, s.now())
		resp, err := s.client().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
			continue
		}
		accepted := resp.StatusCode/100 == 2
		if len(ok) > 0 {
			accepted = false
			for _, c := range ok {
				accepted = accepted || c == resp.StatusCode
			}
		}
		if accepted {
			return resp, nil
		}
		e := parseS3Error(resp)
		resp.Body.Close()
		var se *S3Error
		if errors.As(e, &se) && se.retryable() {
			last = e
			continue
		}
		return nil, e
	}
	return nil, last
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

func notFound(err error) error {
	var se *S3Error
	if errors.As(err, &se) && (se.Status == 404 || se.Code == "NoSuchKey") {
		return ErrNotFound
	}
	return err
}

// ----------------------------------------------------------------- Store

// Path: S3 objects have no local path.
func (s *S3) Path(string) (string, bool) { return "", false }

func (s *S3) Stat(ctx context.Context, digest string) (int64, error) {
	h, err := ParseDigest(digest)
	if err != nil {
		return 0, err
	}
	resp, err := s.do(ctx, http.MethodHead, s.objectURL(s.key(h), nil), nil, nil)
	if err != nil {
		return 0, notFound(err)
	}
	defer drain(resp)
	return resp.ContentLength, nil
}

func (s *S3) Delete(ctx context.Context, digest string) error {
	h, err := ParseDigest(digest)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, s.objectURL(s.key(h), nil), nil, nil, 200, 204, 404)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// PresignGET returns a time-limited URL the client can download from directly.
func (s *S3) PresignGET(digest, filename string, ttl time.Duration) (string, error) {
	h, err := ParseDigest(digest)
	if err != nil {
		return "", err
	}
	extra := url.Values{}
	if filename != "" {
		extra.Set("response-content-disposition", `attachment; filename="`+strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, filename)+`"`)
	}
	extra.Set("response-content-type", "application/octet-stream")
	return presign(http.MethodGet, s.objectURL(s.key(h), nil), extra, s.Creds, s.region(), "s3", s.now(), ttl), nil
}

// Open returns a seekable reader over ranged GETs.
func (s *S3) Open(ctx context.Context, digest string) (ReadSeekCloser, int64, error) {
	size, err := s.Stat(ctx, digest)
	if err != nil {
		return nil, 0, err
	}
	h, _ := ParseDigest(digest)
	return &s3Reader{s: s, ctx: ctx, u: s.objectURL(s.key(h), nil), size: size}, size, nil
}

type s3Reader struct {
	s    *S3
	ctx  context.Context
	u    *url.URL
	size int64
	pos  int64
	body io.ReadCloser
	at   int64 // offset the open body is positioned at
}

func (r *s3Reader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil || r.at != r.pos {
		if r.body != nil {
			r.body.Close()
			r.body = nil
		}
		resp, err := r.s.do(r.ctx, http.MethodGet, r.u, map[string]string{"Range": "bytes=" + strconv.FormatInt(r.pos, 10) + "-"}, nil, 200, 206)
		if err != nil {
			return 0, notFound(err)
		}
		r.body, r.at = resp.Body, r.pos
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	r.at += int64(n)
	if err == io.EOF && r.pos < r.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *s3Reader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += r.pos
	case io.SeekEnd:
		off += r.size
	}
	if off < 0 {
		return 0, errors.New("s3: negative seek")
	}
	r.pos = off
	return off, nil
}

func (r *s3Reader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

// -------------------------------------------------------------------- Put

type part struct {
	n    int
	data []byte
	etag string
}

// Put streams r into the bucket. Objects smaller than one part are uploaded
// with a single PUT directly to their final key; larger ones use a multipart
// upload to a temporary key that is verified and then copied into place.
func (s *S3) Put(ctx context.Context, r io.Reader, expect string, maxSize int64) (string, int64, error) {
	if maxSize > 0 {
		r = io.LimitReader(r, maxSize+1)
	}
	r = ctxReader{ctx, r}
	ps := s.partSize()
	first := make([]byte, ps)
	n, err := io.ReadFull(r, first)
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF: // fits in a single PUT
		data := first[:n]
		if maxSize > 0 && int64(n) > maxSize {
			return "", 0, ErrTooLarge
		}
		sum := sha256.Sum256(data)
		hx := hex.EncodeToString(sum[:])
		digest := "sha256:" + hx
		if expect != "" && expect != digest {
			return "", 0, fmt.Errorf("%w: expected %s got %s", ErrDigestMismatch, expect, digest)
		}
		if sz, err := s.Stat(ctx, digest); err == nil && sz == int64(n) {
			return digest, sz, nil // immutable: already present
		}
		resp, err := s.do(ctx, http.MethodPut, s.objectURL(s.key(hx), nil), map[string]string{"Content-Type": "application/octet-stream"}, data)
		if err != nil {
			return "", 0, err
		}
		drain(resp)
		return digest, int64(n), nil
	case err != nil:
		return "", 0, err
	}
	return s.putMultipart(ctx, first, r, expect, maxSize)
}

func (s *S3) putMultipart(ctx context.Context, first []byte, rest io.Reader, expect string, maxSize int64) (string, int64, error) {
	var rnd [12]byte
	rand.Read(rnd[:])
	tmpKey := s.Prefix + "tmp/upload-" + hex.EncodeToString(rnd[:])
	tmpURL := func(q url.Values) *url.URL { return s.objectURL(tmpKey, q) }

	resp, err := s.do(ctx, http.MethodPost, tmpURL(url.Values{"uploads": {""}}), map[string]string{"Content-Type": "application/octet-stream"}, []byte{})
	if err != nil {
		return "", 0, err
	}
	var init struct{ UploadId string }
	err = xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadId == "" {
		return "", 0, errors.New("s3: bad CreateMultipartUpload response")
	}
	cleanup := func() { // abort the upload and drop anything already assembled
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if resp, err := s.do(c, http.MethodDelete, tmpURL(url.Values{"uploadId": {init.UploadId}}), nil, nil, 200, 204, 404); err == nil {
			drain(resp)
		}
	}

	ps := s.partSize()
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	// Bounded memory: Concurrency+1 part buffers (the first one included)
	// circulate between the reader and the uploaders.
	free := make(chan []byte, s.conc()+1)
	for i := 0; i < s.conc(); i++ {
		free <- nil
	}
	type job struct {
		n    int
		data []byte
	}
	work := make(chan job)
	var (
		mu    sync.Mutex
		done  = map[int]string{}
		upErr error
		wg    sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if upErr == nil {
			upErr = err
		}
		mu.Unlock()
		cancel()
	}
	for i := 0; i < s.conc(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				q := url.Values{"partNumber": {strconv.Itoa(j.n)}, "uploadId": {init.UploadId}}
				resp, err := s.do(ctx2, http.MethodPut, tmpURL(q), nil, j.data)
				if err != nil {
					fail(err)
				} else {
					mu.Lock()
					done[j.n] = resp.Header.Get("ETag")
					mu.Unlock()
					drain(resp)
				}
				free <- j.data
			}
		}()
	}

	h := sha256.New()
	var total int64
	pn := 0
	send := func(data []byte) error {
		pn++
		if pn > maxParts {
			return errors.New("s3: object too large for the configured part size")
		}
		total += int64(len(data))
		if maxSize > 0 && total > maxSize {
			return ErrTooLarge
		}
		h.Write(data)
		select {
		case work <- job{pn, data}:
			return nil
		case <-ctx2.Done():
			return ctx2.Err()
		}
	}
	readErr := send(first)
	for readErr == nil {
		var buf []byte
		select {
		case buf = <-free:
		case <-ctx2.Done():
			readErr = ctx2.Err()
			continue
		}
		if cap(buf) < int(ps) {
			buf = make([]byte, ps)
		}
		n, err := io.ReadFull(rest, buf[:ps])
		if n > 0 {
			readErr = send(buf[:n])
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil && readErr == nil {
			readErr = err
		}
	}
	close(work)
	wg.Wait()
	mu.Lock()
	if upErr != nil && (readErr == nil || errors.Is(readErr, context.Canceled)) {
		readErr = upErr
	}
	mu.Unlock()
	if readErr != nil {
		cleanup()
		return "", 0, readErr
	}

	// Digest check before anything gets a content-addressed name.
	hx := hex.EncodeToString(h.Sum(nil))
	digest := "sha256:" + hx
	if expect != "" && expect != digest {
		cleanup()
		return "", 0, fmt.Errorf("%w: expected %s got %s", ErrDigestMismatch, expect, digest)
	}

	nums := make([]int, 0, len(done))
	for n := range done {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var body bytes.Buffer
	body.WriteString("<CompleteMultipartUpload>")
	for _, n := range nums {
		fmt.Fprintf(&body, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, xmlEscape(done[n]))
	}
	body.WriteString("</CompleteMultipartUpload>")
	resp, err = s.do(ctx, http.MethodPost, tmpURL(url.Values{"uploadId": {init.UploadId}}), map[string]string{"Content-Type": "application/xml"}, body.Bytes())
	if err == nil {
		err = checkBodyError(resp) // S3 may answer 200 with an <Error> body
	}
	if err != nil {
		cleanup()
		return "", 0, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if resp, err := s.do(c, http.MethodDelete, tmpURL(nil), nil, nil, 200, 204, 404); err == nil {
			drain(resp)
		}
	}()
	if sz, err := s.Stat(ctx, digest); err == nil && sz == total {
		return digest, total, nil // already present
	}
	if err := s.copyObject(ctx, tmpKey, s.key(hx), total); err != nil {
		return "", 0, err
	}
	return digest, total, nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// checkBodyError consumes a 2xx response and reports an <Error> document in it.
func checkBodyError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if bytes.Contains(b, []byte("<Error>")) {
		var x struct{ Code, Message string }
		xml.Unmarshal(b, &x)
		return &S3Error{Status: 500, Code: x.Code, Message: x.Message}
	}
	return nil
}

// copyObject copies src to dst inside the bucket (ranged for objects over 4 GiB).
func (s *S3) copyObject(ctx context.Context, src, dst string, size int64) error {
	source := "/" + s.Bucket + "/" + awsEncode(src, false)
	if size <= copyLimit {
		resp, err := s.do(ctx, http.MethodPut, s.objectURL(dst, nil), map[string]string{"x-amz-copy-source": source}, nil)
		if err != nil {
			return err
		}
		return checkBodyError(resp)
	}
	resp, err := s.do(ctx, http.MethodPost, s.objectURL(dst, url.Values{"uploads": {""}}), map[string]string{"Content-Type": "application/octet-stream"}, []byte{})
	if err != nil {
		return err
	}
	var init struct{ UploadId string }
	err = xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadId == "" {
		return errors.New("s3: bad CreateMultipartUpload response")
	}
	var body bytes.Buffer
	body.WriteString("<CompleteMultipartUpload>")
	for off, n := int64(0), 1; off < size; off, n = off+copyRange, n+1 {
		end := off + copyRange - 1
		if end >= size {
			end = size - 1
		}
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {init.UploadId}}
		resp, err := s.do(ctx, http.MethodPut, s.objectURL(dst, q), map[string]string{
			"x-amz-copy-source": source, "x-amz-copy-source-range": fmt.Sprintf("bytes=%d-%d", off, end)}, nil)
		if err == nil {
			var r struct{ ETag string }
			xml.NewDecoder(resp.Body).Decode(&r)
			resp.Body.Close()
			fmt.Fprintf(&body, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, xmlEscape(r.ETag))
		}
		if err != nil {
			s.do(context.WithoutCancel(ctx), http.MethodDelete, s.objectURL(dst, url.Values{"uploadId": {init.UploadId}}), nil, nil, 200, 204, 404)
			return err
		}
	}
	body.WriteString("</CompleteMultipartUpload>")
	resp, err = s.do(ctx, http.MethodPost, s.objectURL(dst, url.Values{"uploadId": {init.UploadId}}), map[string]string{"Content-Type": "application/xml"}, body.Bytes())
	if err != nil {
		return err
	}
	return checkBodyError(resp)
}
