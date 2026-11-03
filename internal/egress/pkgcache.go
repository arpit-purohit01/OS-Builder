package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"foundry.dev/foundry/internal/cache"
)

// PkgCache is the package tier of the build cache (docs/context/caching.md):
// immutable mirror files fetched through the egress proxy are kept on the
// host's disk under a size-aware W-TinyLFU policy. Only content that cannot
// change under the same URL is cached:
//
//   - pool files (.deb/.udeb/.ddeb/.dsc/source tarballs): Debian archives
//     never replace a file of the same name and version;
//   - by-hash index files (…/by-hash/SHA256/<hex>): content-addressed, and the
//     body is verified against the hash before it is stored.
//
// Release/InRelease and other mutable indexes always go upstream, so apt's
// signature checks see current metadata. Packages themselves are verified by
// apt against the signed indexes, so a cache can deny service but never
// substitute content.
type PkgCache struct {
	dir     string
	policy  *cache.TinyLFU
	flight  sync.Map // key → *sync.Mutex (one download per object)
	Hits    atomic.Int64
	Misses  atomic.Int64
	Stored  atomic.Int64
	maxItem int64
}

var (
	poolRe   = regexp.MustCompile(`/pool/[^?#]+\.(deb|udeb|ddeb|dsc|tar\.(gz|xz|bz2|zst)|diff\.gz)$`)
	byHashRe = regexp.MustCompile(`/by-hash/SHA256/([0-9a-f]{64})$`)
)

// NewPkgCache opens (and re-indexes) a cache directory.
func NewPkgCache(dir string, capacity int64) (*PkgCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &PkgCache{dir: dir, policy: cache.NewTinyLFU(capacity, 50000), maxItem: max(capacity/4, 1)}
	c.policy.OnEvict = func(key string, _ int64) { os.Remove(c.path(key)) }
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			c.policy.Add(e.Name(), fi.Size())
		}
	}
	return c, nil
}

// Cacheable reports whether a URL path names immutable content; for by-hash
// paths it returns the expected SHA-256.
func Cacheable(path string) (ok bool, sha string) {
	if m := byHashRe.FindStringSubmatch(path); m != nil {
		return true, m[1]
	}
	return poolRe.MatchString(path), ""
}

func (c *PkgCache) key(host, path string) string {
	h := sha256.Sum256([]byte(strings.ToLower(host) + "\x00" + path))
	return hex.EncodeToString(h[:])
}

func (c *PkgCache) path(key string) string { return filepath.Join(c.dir, key) }

// Used is the cached byte count.
func (c *PkgCache) Used() int64 { return c.policy.Used() }

// serve answers from the cache, or fetches through fetch and stores the body
// while streaming it to the client. It returns bytes sent and whether the
// object came from the cache.
func (c *PkgCache) serve(w http.ResponseWriter, r *http.Request, host string, fetch func() (*http.Response, error)) (int64, bool, error) {
	_, want := Cacheable(r.URL.Path)
	key := c.key(host, r.URL.Path)
	mu, _ := c.flight.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	if f, err := os.Open(c.path(key)); err == nil && c.policy.Get(key) {
		defer f.Close()
		c.Hits.Add(1)
		fi, _ := f.Stat()
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Foundry-Cache", "hit")
		w.WriteHeader(http.StatusOK)
		n, err := io.Copy(w, f)
		return n, true, err
	}
	c.Misses.Add(1)
	resp, err := fetch()
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Foundry-Cache", "miss")
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode != http.StatusOK || resp.ContentLength > c.maxItem {
		n, err := io.Copy(w, resp.Body)
		return n, false, err
	}
	tmp, err := os.CreateTemp(c.dir, ".dl-")
	if err != nil {
		n, err := io.Copy(w, resp.Body)
		return n, false, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(w, io.TeeReader(resp.Body, io.MultiWriter(tmp, h)))
	tmp.Close()
	if err != nil || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return n, false, err // truncated: never stored
	}
	if want != "" && hex.EncodeToString(h.Sum(nil)) != want {
		return n, false, nil // by-hash mismatch: serve-through only
	}
	if os.Rename(tmp.Name(), c.path(key)) == nil {
		if c.policy.Add(key, n) {
			c.Stored.Add(1)
		} else {
			os.Remove(c.path(key))
		}
	}
	return n, false, nil
}
