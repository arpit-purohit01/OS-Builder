package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPkgCache(t *testing.T) {
	var upstream atomic.Int64
	idx := []byte("Package: hello\n")
	sum := sha256.Sum256(idx)
	byHash := "/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, ".deb"):
			w.Write([]byte("DEB-CONTENT"))
		case r.URL.Path == byHash:
			w.Write(idx)
		case strings.Contains(r.URL.Path, "/by-hash/"):
			w.Write([]byte("tampered"))
		default:
			w.Write([]byte("Release v" + r.URL.Query().Get("n")))
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	pc, err := NewPkgCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	pn, _ := strconv.Atoi(port)
	p := &Proxy{Allow: []Rule{{Host: "mirror.test", Ports: []int{pn}}}, Cache: pc,
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) {
			return net.Dial(network, strings.TrimPrefix(srv.URL, "http://"))
		}}
	ps := httptest.NewServer(p)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	cl := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	get := func(path string) (string, string) {
		resp, err := cl.Get("http://mirror.test:" + port + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.Header.Get("X-Foundry-Cache")
	}
	deb := "/ubuntu/pool/main/h/hello/hello_2.10-3_amd64.deb"
	if b, c := get(deb); b != "DEB-CONTENT" || c != "miss" {
		t.Fatal(b, c)
	}
	if b, c := get(deb); b != "DEB-CONTENT" || c != "hit" || upstream.Load() != 1 {
		t.Fatalf("%q %q upstream=%d", b, c, upstream.Load())
	}
	get(byHash)
	if b, c := get(byHash); b != string(idx) || c != "hit" {
		t.Fatal("by-hash not cached", b, c)
	}
	bad := "/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + strings.Repeat("0", 64)
	get(bad)
	if _, c := get(bad); c != "miss" {
		t.Fatal("content not matching its hash was cached")
	}
	n := upstream.Load()
	get("/ubuntu/dists/noble/InRelease")
	get("/ubuntu/dists/noble/InRelease")
	if upstream.Load() != n+2 {
		t.Fatal("mutable index was cached")
	}
	if pc.Hits.Load() != 2 || pc.Stored.Load() != 2 {
		t.Fatalf("hits=%d stored=%d", pc.Hits.Load(), pc.Stored.Load())
	}
	// Restart: entries survive.
	pc2, _ := NewPkgCache(pc.dir, 1<<20)
	if pc2.policy.Len() != 2 {
		t.Fatalf("reindexed %d", pc2.policy.Len())
	}
}

func TestCacheable(t *testing.T) {
	for path, want := range map[string]bool{
		"/ubuntu/pool/main/s/systemd/systemd_255.4-1ubuntu8_amd64.deb": true,
		"/debian/pool/main/h/hello/hello_2.10.orig.tar.gz":             true,
		"/ubuntu/dists/noble/InRelease":                                false,
		"/ubuntu/dists/noble/main/binary-amd64/Packages.xz":            false,
		"/ubuntu/pool/main/x/x/../../../../dists/noble/InRelease":      false,
	} {
		if ok, _ := Cacheable(path); ok != want {
			t.Errorf("%s: %v", path, ok)
		}
	}
}
