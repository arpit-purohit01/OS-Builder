package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/ids"
	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/upload"
)

// uploadKinds: allowed media types and size limits per asset kind.
var uploadKinds = map[string]struct {
	media  []string
	max    int64
	expert bool
}{
	"image":        {[]string{"image/png", "image/jpeg", "image/svg+xml"}, 20 << 20, false},
	"layout":       {[]string{"application/json"}, 1 << 20, false},
	"font":         {[]string{"font/ttf", "font/otf", "font/woff2"}, 10 << 20, false},
	"overlay":      {[]string{"application/x-tar", "application/gzip"}, 2 << 30, true},
	"deb":          {[]string{"application/vnd.debian.binary-package"}, 1 << 30, true},
	"gpg-key":      {[]string{"application/pgp-keys"}, 1 << 20, true},
	"mkosi-dropin": {[]string{"text/plain"}, 64 << 10, true},
	"flatpak":      {[]string{"application/vnd.flatpak"}, 2 << 30, true},
	"script":       {[]string{"text/x-shellscript"}, 1 << 20, true},
}

// MediaExt maps media types to file extensions used inside images.
var MediaExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/svg+xml": ".svg", "application/json": ".json",
	"font/ttf": ".ttf", "font/otf": ".otf", "font/woff2": ".woff2", "application/x-tar": ".tar", "application/gzip": ".tar.gz",
	"application/vnd.debian.binary-package": ".deb", "application/pgp-keys": ".asc", "text/plain": ".conf", "text/x-shellscript": ".sh"}

func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	var req struct {
		Kind      string `json:"kind"`
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		SHA256    string `json:"sha256"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	k, ok := uploadKinds[req.Kind]
	if !ok || !contains(k.media, req.MediaType) {
		problem(w, r, 422, "invalid-upload", "Unsupported file type", "")
		return
	}
	if k.expert && !s.policy(r).ExpertAllowed {
		problem(w, r, 403, "expert-disabled", "Custom packages and overlays need expert mode", "")
		return
	}
	if req.Size <= 0 || req.Size > k.max {
		problem(w, r, 422, "too-large", "File too large", "")
		return
	}
	digest := "sha256:" + strings.TrimPrefix(req.SHA256, "sha256:")
	if _, err := blob.ParseDigest(digest); err != nil {
		problem(w, r, 422, "invalid-digest", "sha256 must be 64 lowercase hex characters", "")
		return
	}
	// The upload id binds org, kind and media type; the URL signature binds digest and size.
	uid := "upl_" + p.OrgID + "." + req.Kind + "." + strings.ReplaceAll(req.MediaType, "/", "~") + "." + ids.Secret(10)
	writeJSON(w, 201, map[string]any{"uploadId": uid, "url": s.signer.UploadURL(uid, digest, req.Size, 30*time.Minute), "digest": digest})
}

func (s *Server) putUpload(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	digest, size, err := s.signer.VerifyUpload(uid, r.URL.Query())
	if err != nil {
		problem(w, r, 403, "bad-signature", "Upload URL invalid or expired", "")
		return
	}
	parts := strings.Split(strings.TrimPrefix(uid, "upl_"), ".")
	if len(parts) != 4 {
		problem(w, r, 400, "bad-upload", "Malformed upload id", "")
		return
	}
	orgID, kind, media := parts[0], parts[1], strings.ReplaceAll(parts[2], "~", "/")
	if r.ContentLength > 0 && r.ContentLength != size {
		problem(w, r, 422, "size-mismatch", "Content-Length does not match the declared size", "")
		return
	}
	got, n, err := s.blobs.Put(r.Context(), r.Body, digest, size)
	if err != nil {
		if errors.Is(err, blob.ErrDigestMismatch) || errors.Is(err, blob.ErrTooLarge) {
			problem(w, r, 422, "integrity", "Uploaded bytes do not match the declared digest/size", "")
			return
		}
		s.fail(w, r, err)
		return
	}
	if n != size {
		problem(w, r, 422, "size-mismatch", "Uploaded size differs from declared size", "")
		return
	}
	rep, err := s.validateAsset(r, got, kind, media)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vb, _ := json.Marshal(rep)
	status := "valid"
	if !rep.Valid {
		status = "rejected"
	}
	a := &store.Asset{Digest: got, OrgID: orgID, Kind: kind, MediaType: media, Size: n, Validation: vb, Status: status}
	if err := s.db.PutAsset(r.Context(), a); err != nil {
		s.fail(w, r, err)
		return
	}
	code := 201
	if status == "rejected" {
		code = 422
	}
	writeJSON(w, code, a)
}

func (s *Server) validateAsset(r *http.Request, digest, kind, media string) (*upload.Report, error) {
	f, _, err := s.blobs.Open(r.Context(), digest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	switch kind {
	case "image":
		data, err := io.ReadAll(io.LimitReader(f, 20<<20))
		if err != nil {
			return nil, err
		}
		return upload.ValidateImage(data, 8192*8192), nil
	case "overlay":
		return upload.ValidateArchive(f, upload.DefaultLimits)
	case "layout":
		data, _ := io.ReadAll(io.LimitReader(f, 1<<20))
		rep := &upload.Report{Kind: "layout", Valid: true}
		if _, err := osspec.ParseLayout(data); err != nil {
			rep.Valid = false
			rep.Findings = append(rep.Findings, upload.Finding{Rule: "LAYOUT", Detail: err.Error()})
		}
		return rep, nil
	case "script":
		data, _ := io.ReadAll(io.LimitReader(f, 1<<20))
		rep := &upload.Report{Kind: "script", Valid: bytes.HasPrefix(data, []byte("#!"))}
		return rep, nil
	case "deb", "gpg-key":
		p, release, err := blob.Local(r.Context(), s.blobs, digest, "")
		if err != nil {
			return nil, err
		}
		defer release()
		if kind == "deb" {
			return upload.ValidateDeb(r.Context(), p), nil
		}
		return upload.ValidateGPGKey(r.Context(), p), nil
	case "mkosi-dropin":
		data, _ := io.ReadAll(io.LimitReader(f, 64<<10))
		return upload.ValidateMkosiDropin(data), nil
	default:
		return &upload.Report{Kind: kind, Valid: true}, nil // deep checks happen in the scanner
	}
}

func (s *Server) getAsset(w http.ResponseWriter, r *http.Request) {
	a, err := s.db.GetAsset(r.Context(), getPrincipal(r.Context()).OrgID, "sha256:"+strings.TrimPrefix(r.PathValue("digest"), "sha256:"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"asset": a, "url": s.signer.DownloadURL(a.Digest, "", 15*time.Minute)})
}

// getBlob serves signed downloads with Range support.
func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	d := r.PathValue("digest")
	if err := s.signer.VerifyDownload(d, r.URL.Query()); err != nil {
		problem(w, r, 403, "bad-signature", "Download link invalid or expired", "")
		return
	}
	name := r.URL.Query().Get("name")
	// S3-style stores serve the bytes themselves: redirect to a short-lived
	// presigned URL so artifacts never stream through the API.
	if ps, ok := s.blobs.(blob.Presigner); ok && r.Method == http.MethodGet && s.cfg.PresignDownloads {
		if _, err := s.blobs.Stat(r.Context(), d); err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				problem(w, r, 404, "not-found", "Not found", "")
				return
			}
			s.fail(w, r, err)
			return
		}
		u, err := ps.PresignGET(d, name, 5*time.Minute)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	f, _, err := s.blobs.Open(r.Context(), d)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			problem(w, r, 404, "not-found", "Not found", "")
			return
		}
		s.fail(w, r, err)
		return
	}
	defer f.Close()
	if name != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.Map(safeFileRune, name)+`"`)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("ETag", `"`+d+`"`)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func safeFileRune(r rune) rune {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
		return r
	}
	return '_'
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
