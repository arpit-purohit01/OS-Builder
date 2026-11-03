package signer

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/store"
)

// Service is the signing service's HTTP API. Every operation is authorized
// against the database: the build must be in state "finalizing" (i.e. it was
// approved), and inputs must be exactly the artifacts that build recorded.
// Outputs are written to the blob store; responses carry digests.
type Service struct {
	DB       *store.DB
	Blobs    blob.Store
	KeysRoot string
	Tools    Tools
	Token    string // shared bearer token of finalize workers
	Log      *slog.Logger

	mu sync.Mutex // serializes key generation
}

// Handler returns the HTTP handler.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /v1/builds/{build}/uki", s.op("uki", s.signUKI))
	mux.HandleFunc("POST /v1/builds/{build}/bootloader", s.op("bootloader", s.bootloader))
	mux.HandleFunc("POST /v1/builds/{build}/verity", s.op("verity", s.verity))
	mux.HandleFunc("POST /v1/builds/{build}/enrollment", s.op("enrollment", s.enrollment))
	mux.HandleFunc("POST /v1/builds/{build}/blob", s.op("blob", s.signBlob))
	mux.HandleFunc("POST /v1/builds/{build}/public-keys", s.op("public-keys", s.publicKeys))
	return mux
}

type opCtx struct {
	build *store.Build
	org   *store.Org
	base  string
	keys  *Keys
	body  map[string]string
}

type opFunc func(ctx context.Context, o *opCtx) (any, error)

// errDenied maps to 403.
type errDenied struct{ msg string }

func (e errDenied) Error() string { return e.msg }

func (s *Service) op(name string, f opFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		var body map[string]string
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
				http.Error(w, `{"error":"bad request"}`, 400)
				return
			}
		}
		ctx := r.Context()
		o, err := s.authorize(ctx, r.PathValue("build"))
		if err == nil {
			o.body = body
			var res any
			res, err = f(ctx, o)
			if err == nil {
				s.Log.Info("signed", "op", name, "build", o.build.ID, "org", o.org.ID)
				detail := map[string]any{"op": name, "result": res}
				if aerr := s.DB.Audit(ctx, o.org.ID, "signer", "signer."+name, o.build.ID, detail); aerr != nil {
					s.Log.Error("audit", "err", aerr)
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(res)
				return
			}
		}
		code := 500
		var d errDenied
		switch {
		case errors.As(err, &d):
			code = 403
		case errors.Is(err, store.ErrNotFound):
			code = 404
		}
		s.Log.Warn("sign refused", "op", name, "build", r.PathValue("build"), "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	}
}

// TenantFor maps an org to its key-set name.
func TenantFor(o *store.Org) string { return strings.ToLower(o.Slug) }

func (s *Service) authorize(ctx context.Context, buildID string) (*opCtx, error) {
	b, err := s.DB.GetBuildAny(ctx, buildID)
	if err != nil {
		return nil, err
	}
	if b.State != store.StateFinalizing {
		return nil, errDenied{fmt.Sprintf("build %s is %s; signing requires an approved build in finalizing", b.ID, b.State)}
	}
	org, err := s.DB.GetOrg(ctx, b.OrgID)
	if err != nil {
		return nil, err
	}
	sv, err := s.DB.GetSpecVersion(ctx, b.OrgID, b.SpecVersionID)
	if err != nil {
		return nil, err
	}
	var spec struct {
		Foundation struct{ Base string } `json:"foundation"`
	}
	json.Unmarshal(sv.Spec, &spec)
	s.mu.Lock()
	keys, err := Open(s.KeysRoot, TenantFor(org), true)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &opCtx{build: b, org: org, base: spec.Foundation.Base, keys: keys}, nil
}

// artifact returns the build's recorded digest for kind.
func (s *Service) artifact(ctx context.Context, o *opCtx, kind string) (string, error) {
	as, err := s.DB.Artifacts(ctx, o.org.ID, o.build.ID)
	if err != nil {
		return "", err
	}
	for _, a := range as {
		if a.Kind == kind {
			return a.Digest, nil
		}
	}
	return "", errDenied{"build has no " + kind + " artifact"}
}

func (s *Service) tmp() (string, func(), error) {
	d, err := os.MkdirTemp("", "foundry-signer-")
	return d, func() { os.RemoveAll(d) }, err
}

func (s *Service) fetch(ctx context.Context, digest, dst string) error {
	r, _, err := s.Blobs.Open(ctx, digest)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := f.ReadFrom(r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Service) putFile(ctx context.Context, p string) (string, error) {
	d, _, err := blob.PutFile(ctx, s.Blobs, p)
	return d, err
}

func (s *Service) putBytes(ctx context.Context, b []byte) (string, error) {
	d, _, err := s.Blobs.Put(ctx, bytes.NewReader(b), "", 0)
	return d, err
}

func (s *Service) signUKI(ctx context.Context, o *opCtx) (any, error) {
	want, err := s.artifact(ctx, o, "uki-unsigned")
	if err != nil {
		return nil, err
	}
	if o.body["digest"] != want {
		return nil, errDenied{"UKI digest does not match the build's artifact"}
	}
	dir, done, err := s.tmp()
	if err != nil {
		return nil, err
	}
	defer done()
	in, out := filepath.Join(dir, "in.efi"), filepath.Join(dir, "out.efi")
	if err := s.fetch(ctx, want, in); err != nil {
		return nil, err
	}
	variant := UKIVariant(o.body["variant"])
	if variant != VariantDefault && variant != VariantInstaller {
		return nil, errDenied{"unknown UKI variant"}
	}
	info, err := o.keys.BuildUKI(ctx, s.Tools, o.base, in, out, variant)
	if err != nil {
		return nil, err
	}
	if o.build.Roothash == "" || info.Usrhash != o.build.Roothash {
		return nil, errDenied{"UKI usrhash= does not match the build's verified usr root hash"}
	}
	d, err := s.putFile(ctx, out)
	return map[string]string{"digest": d, "usrhash": info.Usrhash, "variant": string(variant)}, err
}

func (s *Service) bootloader(ctx context.Context, o *opCtx) (any, error) {
	dir, done, err := s.tmp()
	if err != nil {
		return nil, err
	}
	defer done()
	out := filepath.Join(dir, "boot.efi")
	if err := o.keys.SignBootloader(ctx, s.Tools, o.base, out); err != nil {
		return nil, err
	}
	d, err := s.putFile(ctx, out)
	return map[string]string{"digest": d}, err
}

func (s *Service) verity(ctx context.Context, o *opCtx) (any, error) {
	if o.build.Roothash == "" || o.body["roothash"] != o.build.Roothash {
		return nil, errDenied{"root hash does not match the build"}
	}
	p, err := o.keys.VeritySig(ctx, s.Tools, o.build.Roothash)
	if err != nil {
		return nil, err
	}
	d, err := s.putBytes(ctx, p)
	return map[string]string{"digest": d}, err
}

func (s *Service) enrollment(ctx context.Context, o *opCtx) (any, error) {
	files, err := o.keys.EnrollmentFiles(ctx, s.Tools)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n, b := range files {
		d, err := s.putBytes(ctx, b)
		if err != nil {
			return nil, err
		}
		out[n] = d
	}
	return out, nil
}

// signable artifact kinds for detached Ed25519 signatures.
var signableKinds = map[string]bool{"sha256sums": true, "manifest": true, "provenance": true, "sbom": true}

var sumsLine = regexp.MustCompile(`^[0-9a-f]{64}  [A-Za-z0-9._+-]{1,200}$`)

// checkSums accepts only a well-formed SHA256SUMS that lists the build's
// verified usr partition (file name carries the partition UUID derived from
// the root hash), so the signature binds to this build's /usr.
func checkSums(data []byte, roothash string) error {
	if len(roothash) != 64 {
		return errors.New("build has no verified root hash")
	}
	h := roothash
	want := "_" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32] + ".usr.raw"
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 || len(lines) > 64 {
		return errors.New("SHA256SUMS has an unexpected number of entries")
	}
	found := false
	for _, l := range lines {
		if !sumsLine.MatchString(l) {
			return errors.New("SHA256SUMS is malformed")
		}
		if strings.HasSuffix(l, want) {
			found = true
		}
	}
	if !found {
		return errors.New("SHA256SUMS does not list the build's verified usr partition")
	}
	return nil
}

func (s *Service) signBlob(ctx context.Context, o *opCtx) (any, error) {
	as, err := s.DB.Artifacts(ctx, o.org.ID, o.build.ID)
	if err != nil {
		return nil, err
	}
	ok, kind := false, ""
	for _, a := range as {
		if a.Digest == o.body["digest"] && signableKinds[a.Kind] {
			ok, kind = true, a.Kind
		}
	}
	if !ok {
		return nil, errDenied{"digest is not a signable artifact of this build"}
	}
	r, _, err := s.Blobs.Open(ctx, o.body["digest"])
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, 4096)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) > 16<<20 {
			r.Close()
			return nil, errDenied{"metadata too large to sign"}
		}
		if err != nil {
			break
		}
	}
	r.Close()
	if kind == "sha256sums" {
		if err := checkSums(data, o.build.Roothash); err != nil {
			return nil, errDenied{err.Error()}
		}
	}
	sig, err := o.keys.SignBlob(data)
	if err != nil {
		return nil, err
	}
	d, err := s.putBytes(ctx, sig)
	return map[string]string{"digest": d}, err
}

func (s *Service) publicKeys(ctx context.Context, o *opCtx) (any, error) {
	out := map[string]string{}
	for name, p := range map[string]string{"db.crt": o.keys.CertPath(RoleDB), "kek.crt": o.keys.CertPath(RoleKEK), "pk.crt": o.keys.CertPath(RolePK),
		"pcr.pub.pem": o.keys.CertPath(RolePCR), "artifact.pub.pem": filepath.Join(o.keys.Dir, "artifact.pub.pem")} {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		d, err := s.putBytes(ctx, b)
		if err != nil {
			return nil, err
		}
		out[name] = d
	}
	return out, nil
}
