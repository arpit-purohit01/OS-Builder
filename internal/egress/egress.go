// Package egress is the build egress proxy (Phase 1, P1-08). Build sandboxes
// reach the network only through it. It forwards plain HTTP requests and
// HTTPS CONNECT tunnels to allowlisted hosts and ports, attributes every
// request to a build via per-build proxy credentials, and reports denied
// attempts (a hard blocker in risk scoring: "egress to a non-allowlisted
// host during build").
package egress

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rule allows a host pattern ("archive.ubuntu.com", "*.ubuntu.com") on ports.
type Rule struct {
	Host  string
	Ports []int
}

// DefaultAllowlist: package mirrors the bases use (http) plus their https variants.
var DefaultAllowlist = []Rule{
	{"archive.ubuntu.com", []int{80, 443}},
	{"security.ubuntu.com", []int{80, 443}},
	{"ports.ubuntu.com", []int{80, 443}},
	{"snapshot.ubuntu.com", []int{80, 443}},
	{"deb.debian.org", []int{80, 443}},
	{"snapshot.debian.org", []int{80, 443}},
	{"dl.flathub.org", []int{443}},
}

// Event is one proxied or denied request.
type Event struct {
	Time    time.Time `json:"time"`
	BuildID string    `json:"buildId,omitempty"`
	Method  string    `json:"method"`
	Host    string    `json:"host"`
	Port    int       `json:"port"`
	Allowed bool      `json:"allowed"`
	Reason  string    `json:"reason,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
}

// Proxy is an allowlisting forward proxy.
type Proxy struct {
	Allow   []Rule
	Secret  []byte // HMAC key for per-build credentials; empty = no authentication
	Log     *slog.Logger
	OnEvent func(Event) // receives every request (e.g. to store denials as findings)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// DenyPrivate refuses destinations that resolve to loopback/private/link-local
	// addresses (no pivoting into the platform network via DNS tricks).
	DenyPrivate bool
	// Cache keeps immutable package files (optional; see PkgCache).
	Cache *PkgCache

	once sync.Once
	tr   *http.Transport
}

// Credentials returns the proxy user/password for a build.
func (p *Proxy) Credentials(buildID string) (user, pass string) {
	m := hmac.New(sha256.New, p.Secret)
	m.Write([]byte("egress|" + buildID))
	return buildID, hex.EncodeToString(m.Sum(nil))[:32]
}

// URL returns the proxy URL with credentials embedded for a build.
func (p *Proxy) URL(addr, buildID string) string {
	u := &url.URL{Scheme: "http", Host: addr}
	if len(p.Secret) > 0 {
		user, pass := p.Credentials(buildID)
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

func (p *Proxy) authenticate(r *http.Request) (string, bool) {
	if len(p.Secret) == 0 {
		return "", true
	}
	h := r.Header.Get("Proxy-Authorization")
	raw, ok := strings.CutPrefix(h, "Basic ")
	if !ok {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", false
	}
	user, pass, ok := strings.Cut(string(b), ":")
	if !ok {
		return "", false
	}
	_, want := p.Credentials(user)
	return user, hmac.Equal([]byte(pass), []byte(want))
}

// Allowed reports whether host:port matches the allowlist.
func (p *Proxy) Allowed(host string, port int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range p.Allow {
		h := strings.ToLower(r.Host)
		match := h == host
		if s, ok := strings.CutPrefix(h, "*."); ok {
			match = strings.HasSuffix(host, "."+s)
		}
		if !match {
			continue
		}
		for _, pt := range r.Ports {
			if pt == port {
				return true
			}
		}
	}
	return false
}

func (p *Proxy) emit(e Event) {
	e.Time = time.Now().UTC()
	if p.Log != nil {
		lvl := slog.LevelInfo
		if !e.Allowed {
			lvl = slog.LevelWarn
		}
		p.Log.Log(context.Background(), lvl, "egress", "build", e.BuildID, "method", e.Method, "host", e.Host, "port", e.Port, "allowed", e.Allowed, "reason", e.Reason)
	}
	if p.OnEvent != nil {
		p.OnEvent(e)
	}
}

var errPrivate = errors.New("destination resolves to a private address")

func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.DenyPrivate {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() {
				return nil, errPrivate
			}
		}
		addr = net.JoinHostPort(ips[0].IP.String(), port) // connect to the address we checked
	}
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	var d net.Dialer
	d.Timeout = 30 * time.Second
	return d.DialContext(ctx, network, addr)
}

func (p *Proxy) transport() *http.Transport {
	p.once.Do(func() {
		p.tr = &http.Transport{Proxy: nil, DialContext: p.dial, MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second, DisableCompression: true}
	})
	return p.tr
}

func hostPort(hostport string, def int) (string, int) {
	h, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, def
	}
	pt, _ := strconv.Atoi(ps)
	return h, pt
}

// ServeHTTP implements the proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	build, ok := p.authenticate(r)
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="foundry-egress"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		p.emit(Event{Method: r.Method, Host: r.Host, Allowed: false, Reason: "unauthenticated"})
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r, build)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "only absolute http:// URLs or CONNECT are proxied", http.StatusBadRequest)
		return
	}
	host, port := hostPort(r.URL.Host, 80)
	if !p.Allowed(host, port) {
		p.emit(Event{BuildID: build, Method: r.Method, Host: host, Port: port, Reason: "not allowlisted"})
		http.Error(w, "egress to "+host+" is not allowed for builds", http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	if ok, _ := Cacheable(r.URL.Path); ok && p.Cache != nil && r.Method == http.MethodGet && r.Header.Get("Range") == "" {
		out.Header.Del("If-Modified-Since")
		out.Header.Del("If-None-Match")
		n, hit, err := p.Cache.serve(w, r, host, func() (*http.Response, error) { return p.transport().RoundTrip(out) })
		reason := ""
		if hit {
			reason = "cache hit"
		}
		if err != nil {
			reason = "upstream error: " + err.Error()
			if n == 0 && !hit {
				http.Error(w, "upstream error", http.StatusBadGateway)
			}
		}
		p.emit(Event{BuildID: build, Method: r.Method, Host: host, Port: port, Allowed: true, Bytes: n, Reason: reason})
		return
	}
	resp, err := p.transport().RoundTrip(out)
	if err != nil {
		p.emit(Event{BuildID: build, Method: r.Method, Host: host, Port: port, Allowed: true, Reason: "upstream error: " + err.Error()})
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	p.emit(Event{BuildID: build, Method: r.Method, Host: host, Port: port, Allowed: true, Bytes: n})
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, build string) {
	host, port := hostPort(r.Host, 443)
	if !p.Allowed(host, port) {
		p.emit(Event{BuildID: build, Method: "CONNECT", Host: host, Port: port, Reason: "not allowlisted"})
		http.Error(w, "egress to "+host+" is not allowed for builds", http.StatusForbidden)
		return
	}
	up, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		reason := "dial: " + err.Error()
		p.emit(Event{BuildID: build, Method: "CONNECT", Host: host, Port: port, Allowed: errors.Is(err, errPrivate) == false, Reason: reason})
		http.Error(w, "cannot reach "+host, http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	var n int64
	done := make(chan struct{}, 2)
	go func() {
		if buf.Reader.Buffered() > 0 {
			b, _ := buf.Reader.Peek(buf.Reader.Buffered())
			up.Write(b)
		}
		io.Copy(up, conn)
		done <- struct{}{}
	}()
	go func() { n, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
	conn.Close()
	up.Close()
	<-done
	p.emit(Event{BuildID: build, Method: "CONNECT", Host: host, Port: port, Allowed: true, Bytes: n})
}
