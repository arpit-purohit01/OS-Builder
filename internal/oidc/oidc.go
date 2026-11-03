// Package oidc is a small OpenID Connect relying-party client: discovery,
// authorization code + PKCE, ID token verification (RS256/ES256) with a
// cached JWKS that refreshes on unknown key ids.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider configuration.
type Provider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	HTTP         *http.Client
	Now          func() time.Time

	mu       sync.Mutex
	meta     *metadata
	keys     map[string]crypto.PublicKey
	keysTime time.Time
}

type metadata struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
}

// Claims extracted from a verified ID token.
type Claims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Nonce         string `json:"nonce"`
	Issuer        string `json:"iss"`
	Expiry        int64  `json:"exp"`
	IssuedAt      int64  `json:"iat"`
	Audience      any    `json:"aud"`
}

func (p *Provider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) getJSON(ctx context.Context, u string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("oidc: GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// Discover loads provider metadata (cached).
func (p *Provider) Discover(ctx context.Context) error {
	p.mu.Lock()
	if p.meta != nil {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	var m metadata
	if err := p.getJSON(ctx, strings.TrimSuffix(p.Issuer, "/")+"/.well-known/openid-configuration", &m); err != nil {
		return err
	}
	if strings.TrimSuffix(m.Issuer, "/") != strings.TrimSuffix(p.Issuer, "/") {
		return fmt.Errorf("oidc: issuer mismatch %q", m.Issuer)
	}
	p.mu.Lock()
	p.meta = &m
	p.mu.Unlock()
	return nil
}

// PKCE returns a verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomString returns a URL-safe random token.
func RandomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL builds the authorization request URL.
func (p *Provider) AuthURL(state, nonce, challenge string) string {
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	v := url.Values{
		"response_type": {"code"}, "client_id": {p.ClientID}, "redirect_uri": {p.RedirectURL},
		"scope": {strings.Join(scopes, " ")}, "state": {state}, "nonce": {nonce},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	return p.meta.AuthEndpoint + "?" + v.Encode()
}

// Exchange trades the code for tokens and verifies the ID token.
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (*Claims, error) {
	if err := p.Discover(ctx); err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {p.RedirectURL},
		"client_id": {p.ClientID}, "code_verifier": {verifier}}
	req, _ := http.NewRequestWithContext(ctx, "POST", p.meta.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(p.ClientSecret))
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 || tok.IDToken == "" {
		return nil, fmt.Errorf("oidc: token endpoint: %s %s", resp.Status, tok.Error)
	}
	return p.Verify(ctx, tok.IDToken, nonce)
}

// Verify checks signature, issuer, audience, expiry and nonce.
func (p *Provider) Verify(ctx context.Context, raw, nonce string) (*Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: malformed token")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var hdr struct {
		Alg, Kid string
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, err
	}
	key, err := p.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch hdr.Alg {
	case "RS256":
		k, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) != nil {
			return nil, errors.New("oidc: bad signature")
		}
	case "ES256":
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || len(sig) != 64 || !ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return nil, errors.New("oidc: bad signature")
		}
	default:
		return nil, fmt.Errorf("oidc: unsupported alg %q", hdr.Alg)
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, err
	}
	now := p.now().Unix()
	switch {
	case strings.TrimSuffix(c.Issuer, "/") != strings.TrimSuffix(p.Issuer, "/"):
		return nil, errors.New("oidc: wrong issuer")
	case !audienceHas(c.Audience, p.ClientID):
		return nil, errors.New("oidc: wrong audience")
	case c.Expiry < now-60:
		return nil, errors.New("oidc: token expired")
	case c.IssuedAt > now+300:
		return nil, errors.New("oidc: token from the future")
	case nonce != "" && c.Nonce != nonce:
		return nil, errors.New("oidc: nonce mismatch")
	}
	return &c, nil
}

func audienceHas(aud any, id string) bool {
	switch a := aud.(type) {
	case string:
		return a == id
	case []any:
		for _, x := range a {
			if x == id {
				return true
			}
		}
	}
	return false
}

func (p *Provider) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	p.mu.Lock()
	k, ok := p.keys[kid]
	fresh := p.now().Sub(p.keysTime) < time.Minute
	p.mu.Unlock()
	if ok {
		return k, nil
	}
	if fresh && p.keys != nil {
		return nil, fmt.Errorf("oidc: unknown key id %q", kid)
	}
	if err := p.Discover(ctx); err != nil {
		return nil, err
	}
	var set struct {
		Keys []struct {
			Kty, Kid, N, E, Crv, X, Y string
		} `json:"keys"`
	}
	if err := p.getJSON(ctx, p.meta.JWKSURI, &set); err != nil {
		return nil, err
	}
	keys := map[string]crypto.PublicKey{}
	for _, jk := range set.Keys {
		switch jk.Kty {
		case "RSA":
			n, e1 := base64.RawURLEncoding.DecodeString(jk.N)
			e, e2 := base64.RawURLEncoding.DecodeString(jk.E)
			if e1 == nil && e2 == nil {
				keys[jk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
			}
		case "EC":
			if jk.Crv != "P-256" {
				continue
			}
			x, e1 := base64.RawURLEncoding.DecodeString(jk.X)
			y, e2 := base64.RawURLEncoding.DecodeString(jk.Y)
			if e1 == nil && e2 == nil {
				keys[jk.Kid] = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			}
		}
	}
	p.mu.Lock()
	p.keys, p.keysTime = keys, p.now()
	p.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("oidc: unknown key id %q", kid)
}
