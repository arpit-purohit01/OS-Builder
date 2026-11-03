package blob

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4 (header and query-string forms), enough for S3.

// Credentials are static access keys (a session token is optional).
type Credentials struct{ AccessKey, SecretKey, SessionToken string }

const (
	amzDate     = "20060102T150405Z"
	amzDay      = "20060102"
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// awsEncode is the RFC 3986 percent-encoding SigV4 requires.
func awsEncode(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !slash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEncode(k, true)+"="+awsEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// canonicalRequest builds the string to sign. headers are lower-case names.
func canonicalRequest(method, uri string, q url.Values, headers map[string]string, payloadHash string) (canon, signed string) {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var hb strings.Builder
	for _, k := range names {
		hb.WriteString(k + ":" + strings.Join(strings.Fields(headers[k]), " ") + "\n")
	}
	signed = strings.Join(names, ";")
	return strings.Join([]string{method, uri, canonicalQuery(q), hb.String(), signed, payloadHash}, "\n"), signed
}

func signingKey(secret, day, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func stringToSign(t time.Time, scope, canon string) string {
	return "AWS4-HMAC-SHA256\n" + t.UTC().Format(amzDate) + "\n" + scope + "\n" + hexSHA256([]byte(canon))
}

// signRequest adds Authorization (and the x-amz-* headers it signs) to req.
// The request's URL path must already be the canonical (encoded) path.
func signRequest(req *http.Request, c Credentials, region, service, payloadHash string, t time.Time) {
	req.Header.Set("x-amz-date", t.UTC().Format(amzDate))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if c.SessionToken != "" {
		req.Header.Set("x-amz-security-token", c.SessionToken)
	}
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5" || lk == "range" {
			headers[lk] = strings.Join(v, ",")
		}
	}
	canon, signed := canonicalRequest(req.Method, req.URL.EscapedPath(), req.URL.Query(), headers, payloadHash)
	day := t.UTC().Format(amzDay)
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, day, region, service), stringToSign(t, scope, canon)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

// presign returns a query-string-signed URL for a GET/HEAD valid for ttl.
func presign(method string, u *url.URL, extra url.Values, c Credentials, region, service string, t time.Time, ttl time.Duration) string {
	day := t.UTC().Format(amzDay)
	scope := day + "/" + region + "/" + service + "/aws4_request"
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", c.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", t.UTC().Format(amzDate))
	q.Set("X-Amz-Expires", itoa(int64(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	if c.SessionToken != "" {
		q.Set("X-Amz-Security-Token", c.SessionToken)
	}
	canon, _ := canonicalRequest(method, u.EscapedPath(), q, map[string]string{"host": u.Host}, "UNSIGNED-PAYLOAD")
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, day, region, service), stringToSign(t, scope, canon)))
	out := *u
	out.RawQuery = canonicalQuery(q) + "&X-Amz-Signature=" + sig
	return out.String()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
