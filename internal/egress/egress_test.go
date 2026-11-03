package egress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxyAllowDenyAndAttribution(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pkg:"+r.URL.Path) }))
	defer origin.Close()
	ou, _ := url.Parse(origin.URL)
	var mu sync.Mutex
	var events []Event
	p := &Proxy{Secret: []byte("k"), Allow: []Rule{{"mirror.example", []int{80}}, {"*.cdn.example", []int{443}}},
		OnEvent: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
		Dial: func(ctx context.Context, n, a string) (net.Conn, error) { // every name resolves to the test origin
			var d net.Dialer
			return d.DialContext(ctx, n, ou.Host)
		}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	pu, _ := url.Parse(p.URL(strings.TrimPrefix(srv.URL, "http://"), "bld_1"))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get("http://mirror.example/ubuntu/Release")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "pkg:/ubuntu/Release" {
		t.Fatalf("allowed: %d %s", resp.StatusCode, b)
	}
	resp, _ = c.Get("http://evil.example/payload")
	if resp.StatusCode != 403 {
		t.Fatalf("denied host: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = c.Get("http://mirror.example:8080/x")
	if resp.StatusCode != 403 {
		t.Fatalf("denied port: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// wrong credentials
	bad, _ := url.Parse(strings.Replace(pu.String(), "bld_1:", "bld_1:x", 1))
	c2 := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(bad)}}
	resp, _ = c2.Get("http://mirror.example/x")
	if resp.StatusCode != 407 {
		t.Fatalf("bad credentials: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// CONNECT tunnel to an allowlisted wildcard host (plain TCP to the origin through the tunnel)
	conn, _ := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	user, pass := p.Credentials("bld_1")
	req, _ := http.NewRequest("CONNECT", "http://a.cdn.example:443", nil)
	req.Host = "a.cdn.example:443"
	req.SetBasicAuth(user, pass)
	req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
	req.Write(conn)
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("connect: %q", buf[:n])
	}
	io.WriteString(conn, "GET /tunnel HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	all, _ := io.ReadAll(conn)
	if !strings.Contains(string(all), "pkg:/tunnel") {
		t.Fatalf("tunnel: %q", all)
	}
	// The tunnel's event is emitted when it closes, asynchronously.
	var denied, allowed int
	for i := 0; i < 100; i++ {
		mu.Lock()
		n := 0
		for _, e := range events {
			if e.Allowed {
				n++
			}
		}
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.Allowed {
			allowed++
		} else {
			denied++
			if e.BuildID != "bld_1" && e.Reason != "unauthenticated" {
				t.Errorf("denial not attributed: %+v", e)
			}
		}
	}
	if allowed < 2 || denied != 3 {
		t.Fatalf("events allowed=%d denied=%d: %+v", allowed, denied, events)
	}
}

func TestDenyPrivate(t *testing.T) {
	p := &Proxy{Allow: []Rule{{"localhost", []int{80}}}, DenyPrivate: true}
	if _, err := p.dial(context.Background(), "tcp", "localhost:80"); err != errPrivate {
		t.Fatalf("loopback dial: %v", err)
	}
	if !p.Allowed("LOCALHOST.", 80) || p.Allowed("localhost", 81) {
		t.Fatal("allow matching")
	}
}
