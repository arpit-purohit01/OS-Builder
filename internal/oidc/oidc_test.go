package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	nonce string
	aud   string
	exp   time.Time
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIdP) token() string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
	c, _ := json.Marshal(map[string]any{"iss": f.srv.URL, "aud": f.aud, "sub": "u-1", "email": "ada@example.com", "email_verified": true,
		"name": "Ada", "nonce": f.nonce, "exp": f.exp.Unix(), "iat": time.Now().Unix()})
	in := b64(h) + "." + b64(c)
	d := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, d[:])
	return in + "." + b64(sig)
}

func newIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{aud: "client", exp: time.Now().Add(time.Hour)}
	f.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1",
			"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": f.token()})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestCodeFlow(t *testing.T) {
	f := newIdP(t)
	p := &Provider{Issuer: f.srv.URL, ClientID: "client", RedirectURL: "https://app/cb"}
	ctx := context.Background()
	if err := p.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	ver, ch := PKCE()
	u, _ := url.Parse(p.AuthURL("st", "n1", ch))
	if u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("nonce") != "n1" {
		t.Fatal(u)
	}
	f.nonce = "n1"
	c, err := p.Exchange(ctx, "good-code", ver, "n1")
	if err != nil || c.Email != "ada@example.com" || c.Subject != "u-1" {
		t.Fatal(err, c)
	}
	if _, err := p.Exchange(ctx, "bad", ver, "n1"); err == nil {
		t.Fatal("bad code accepted")
	}
	if _, err := p.Verify(ctx, f.token(), "other-nonce"); err == nil {
		t.Fatal("nonce mismatch accepted")
	}
	f.aud = "someone-else"
	if _, err := p.Verify(ctx, f.token(), "n1"); err == nil {
		t.Fatal("wrong audience accepted")
	}
	f.aud, f.exp = "client", time.Now().Add(-time.Hour)
	if _, err := p.Verify(ctx, f.token(), "n1"); err == nil {
		t.Fatal("expired accepted")
	}
	f.exp = time.Now().Add(time.Hour)
	tok := f.token()
	tampered := tok[:strings.LastIndex(tok, ".")] + ".AAAA"
	if _, err := p.Verify(ctx, tampered, "n1"); err == nil {
		t.Fatal("bad signature accepted")
	}
}
