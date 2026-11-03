// Package envelope signs and verifies build job envelopes (Ed25519). The API
// signs what it compiled; workers refuse jobs whose envelope does not verify,
// has expired, or does not match the inputs they fetched — so a tampered
// database row or blob cannot inject a different tree into a build.
package envelope

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"foundry.dev/foundry/internal/jcs"
)

// Envelope describes one build job.
type Envelope struct {
	BuildID        string    `json:"buildId"`
	OrgID          string    `json:"orgId"`
	SpecDigest     string    `json:"specDigest"`
	TreeDigest     string    `json:"treeDigest"`
	CatalogVersion string    `json:"catalogVersion"`
	Assets         []string  `json:"assets,omitempty"`
	Formats        []string  `json:"formats,omitempty"`
	IssuedAt       time.Time `json:"issuedAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Nonce          string    `json:"nonce"`
}

// Signed is the wire form.
type Signed struct {
	Payload   []byte `json:"payload"` // canonical JSON (RFC 8785), base64 in JSON so storage (jsonb) cannot re-serialize it
	KeyID     string `json:"keyId"`
	Signature string `json:"signature"` // base64
}

// KeyID is the first 16 hex chars of sha256(public key).
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Sign produces a signed envelope.
func Sign(priv ed25519.PrivateKey, e Envelope) (*Signed, error) {
	if e.Nonce == "" {
		b := make([]byte, 12)
		rand.Read(b)
		e.Nonce = hex.EncodeToString(b)
	}
	payload, err := jcs.Marshal(e)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, append([]byte("foundry-envelope-v1\n"), payload...))
	return &Signed{Payload: payload, KeyID: KeyID(priv.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(sig)}, nil
}

// Errors.
var (
	ErrSignature = errors.New("envelope: bad signature")
	ErrExpired   = errors.New("envelope: expired")
	ErrUnknown   = errors.New("envelope: unknown signing key")
)

// Verify checks the signature against any of the trusted keys (rotation)
// and the validity window.
func Verify(trusted []ed25519.PublicKey, s *Signed, now time.Time) (*Envelope, error) {
	var key ed25519.PublicKey
	for _, k := range trusted {
		if KeyID(k) == s.KeyID {
			key = k
		}
	}
	if key == nil {
		return nil, ErrUnknown
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil || !ed25519.Verify(key, append([]byte("foundry-envelope-v1\n"), s.Payload...), sig) {
		return nil, ErrSignature
	}
	var e Envelope
	if err := json.Unmarshal(s.Payload, &e); err != nil {
		return nil, err
	}
	if now.After(e.ExpiresAt) || now.Before(e.IssuedAt.Add(-5*time.Minute)) {
		return nil, ErrExpired
	}
	return &e, nil
}

// KeyFromSeed decodes a base64 32-byte seed.
func KeyFromSeed(b64 string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("envelope: key must be a base64 %d-byte seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKeys decodes a comma-separated list of base64 public keys.
func PublicKeys(list string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("envelope: bad public key %q", s)
		}
		out = append(out, ed25519.PublicKey(b))
	}
	return out, nil
}

// DevKey derives a development key from a passphrase (never use in production).
func DevKey(pass string) ed25519.PrivateKey {
	h := sha256.Sum256([]byte("foundry-dev-envelope|" + pass))
	return ed25519.NewKeyFromSeed(h[:])
}
