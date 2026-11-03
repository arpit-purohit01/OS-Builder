package envelope

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	k := DevKey("x")
	other := DevKey("y")
	now := time.Now()
	s, err := Sign(k, Envelope{BuildID: "bld_1", TreeDigest: "sha256:" + strings.Repeat("a", 64), IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	trusted := []ed25519.PublicKey{other.Public().(ed25519.PublicKey), k.Public().(ed25519.PublicKey)}
	e, err := Verify(trusted, s, now)
	if err != nil || e.BuildID != "bld_1" || e.Nonce == "" {
		t.Fatalf("%v %+v", err, e)
	}
	if _, err := Verify([]ed25519.PublicKey{other.Public().(ed25519.PublicKey)}, s, now); err != ErrUnknown {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := Verify(trusted, s, now.Add(2*time.Hour)); err != ErrExpired {
		t.Fatalf("expiry: %v", err)
	}
	tampered := *s
	tampered.Payload = json.RawMessage(strings.Replace(string(s.Payload), "bld_1", "bld_2", 1))
	if _, err := Verify(trusted, &tampered, now); err != ErrSignature {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := KeyFromSeed("bad"); err == nil {
		t.Fatal("bad seed")
	}
}

// TestSurvivesJSONNormalization guards the jsonb round trip: Postgres
// re-serializes JSON (key order, whitespace), which must not break signatures.
func TestSurvivesJSONNormalization(t *testing.T) {
	k := DevKey("x")
	now := time.Now()
	s, err := Sign(k, Envelope{BuildID: "bld_1", OrgID: "org_1", Assets: []string{"sha256:b", "sha256:a"}, IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	var m map[string]any
	json.Unmarshal(b, &m)
	b2, _ := json.MarshalIndent(m, "", "  ")
	var s2 Signed
	if err := json.Unmarshal(b2, &s2); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify([]ed25519.PublicKey{k.Public().(ed25519.PublicKey)}, &s2, now); err != nil {
		t.Fatal(err)
	}
}
