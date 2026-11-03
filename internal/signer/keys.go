// Package signer holds the platform's signing keys and the operations that
// use them: Secure Boot (PE/Authenticode via sbsign), UKI assembly with a
// signed TPM2 PCR policy (ukify/systemd-measure), dm-verity root hash
// signatures (PKCS#7), Secure Boot enrollment variables, and Ed25519
// signatures over release metadata.
//
// Invariant 3 (CLAUDE.md): only cmd/signer imports this package. Keys are
// software keys in a 0700 directory per tenant; a PKCS#11/HSM backend
// implements the same Keys operations in production.
package signer

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Role names of the per-tenant keys.
const (
	RolePK       = "pk"
	RoleKEK      = "kek"
	RoleDB       = "db"  // signs bootloader, UKIs and verity root hashes
	RolePCR      = "pcr" // signs TPM2 PCR 11 policies
	RoleArtifact = "artifact"
)

var tenantRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Keys is one tenant's key set on disk.
type Keys struct {
	Dir    string
	Tenant string
}

func (k *Keys) path(name string) string { return filepath.Join(k.Dir, name) }

// KeyPath returns the private key file for a role (used by external tools).
func (k *Keys) KeyPath(role string) string { return k.path(role + ".key") }

// CertPath returns the certificate (or public key for pcr) for a role.
func (k *Keys) CertPath(role string) string {
	if role == RolePCR {
		return k.path("pcr.pub.pem")
	}
	return k.path(role + ".crt")
}

// OwnerGUID is the EFI signature owner derived from the tenant name.
func (k *Keys) OwnerGUID() string {
	h := sha256.Sum256([]byte("foundry-efi-owner|" + k.Tenant))
	x := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-4%s-8%s-%s", x[0:8], x[8:12], x[13:16], x[17:20], x[20:32])
}

// Open loads (and when create is set, generates) a tenant's keys under root.
func Open(root, tenant string, create bool) (*Keys, error) {
	if !tenantRe.MatchString(tenant) {
		return nil, fmt.Errorf("signer: invalid tenant %q", tenant)
	}
	k := &Keys{Dir: filepath.Join(root, tenant), Tenant: tenant}
	if _, err := os.Stat(k.KeyPath(RoleDB)); err == nil {
		return k, nil
	} else if !create {
		return nil, fmt.Errorf("signer: no keys for tenant %s", tenant)
	}
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := k.generate(); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *Keys) generate() error {
	// PK (self-signed) → KEK → db, like a real platform hierarchy.
	pkKey, pkCert, err := k.rsaCert("PK", nil, nil, true)
	if err != nil {
		return err
	}
	kekKey, kekCert, err := k.rsaCert("KEK", pkKey, pkCert, true)
	if err != nil {
		return err
	}
	if _, _, err := k.rsaCert("db", kekKey, kekCert, false); err != nil {
		return err
	}
	pcr, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	if err := writePEM(k.KeyPath(RolePCR), "PRIVATE KEY", mustPKCS8(pcr), 0o600); err != nil {
		return err
	}
	pub, _ := x509.MarshalPKIXPublicKey(&pcr.PublicKey)
	if err := writePEM(k.CertPath(RolePCR), "PUBLIC KEY", pub, 0o644); err != nil {
		return err
	}
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writePEM(k.KeyPath(RoleArtifact), "PRIVATE KEY", mustPKCS8(edPriv), 0o600); err != nil {
		return err
	}
	edPub, _ := x509.MarshalPKIXPublicKey(edPriv.Public())
	return writePEM(k.path("artifact.pub.pem"), "PUBLIC KEY", edPub, 0o644)
}

func (k *Keys) rsaCert(cn string, parentKey *rsa.PrivateKey, parent *x509.Certificate, ca bool) (*rsa.PrivateKey, *x509.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Foundry " + k.Tenant + " " + cn, Organization: []string{"Foundry tenant " + k.Tenant}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(15, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  ca,
	}
	if ca {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}
	}
	signer, issuer := crypto.Signer(key), tmpl
	if parentKey != nil {
		signer, issuer = parentKey, parent
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, &key.PublicKey, signer)
	if err != nil {
		return nil, nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	role := map[string]string{"PK": RolePK, "KEK": RoleKEK, "db": RoleDB}[cn]
	if err := writePEM(k.KeyPath(role), "PRIVATE KEY", mustPKCS8(key), 0o600); err != nil {
		return nil, nil, err
	}
	if err := writePEM(k.CertPath(role), "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	return key, cert, nil
}

func mustPKCS8(k any) []byte {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return b
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Cert parses a role's certificate.
func (k *Keys) Cert(role string) (*x509.Certificate, error) {
	b, err := os.ReadFile(k.CertPath(role))
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("signer: bad certificate PEM")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func (k *Keys) artifactKey() (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(k.KeyPath(RoleArtifact))
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("signer: bad artifact key")
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("signer: artifact key is not ed25519")
	}
	return ed, nil
}

// ArtifactPublicKey returns the Ed25519 public key.
func (k *Keys) ArtifactPublicKey() (ed25519.PublicKey, error) {
	priv, err := k.artifactKey()
	if err != nil {
		return nil, err
	}
	return priv.Public().(ed25519.PublicKey), nil
}

// SignBlob signs data with the tenant's artifact key (Ed25519).
func (k *Keys) SignBlob(data []byte) ([]byte, error) {
	priv, err := k.artifactKey()
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, data), nil
}

// VerifyBlob checks an artifact signature with a PEM public key.
func VerifyBlob(pubPEM, data, sig []byte) bool {
	blk, _ := pem.Decode(pubPEM)
	if blk == nil {
		return false
	}
	pk, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return false
	}
	ed, ok := pk.(ed25519.PublicKey)
	return ok && ed25519.Verify(ed, data, sig)
}
