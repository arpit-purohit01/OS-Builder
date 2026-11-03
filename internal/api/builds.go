package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/convert"
	"foundry.dev/foundry/internal/envelope"
	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/specsvc"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/upload"
)

func (s *Server) createBuild(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	body, err := readAllLimited(r.Body, 64<<10)
	if err != nil {
		problem(w, r, 400, "bad-request", "Body too large", "")
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	reqHash := sha256.Sum256(body)
	if idemKey != "" {
		if rec, err := s.db.GetIdempotency(r.Context(), p.OrgID, idemKey); err == nil {
			if !bytes.Equal(rec.RequestHash, reqHash[:]) {
				problem(w, r, 409, "idempotency-mismatch", "Idempotency-Key was used with a different request", "")
				return
			}
			w.Header().Set("Idempotent-Replayed", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Status)
			w.Write(rec.Response)
			return
		}
	}
	var req struct {
		SpecVersionID string   `json:"specVersionId"`
		Priority      int      `json:"priority"`
		Formats       []string `json:"formats"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.SpecVersionID == "" {
		problem(w, r, 400, "bad-request", "specVersionId required", "")
		return
	}
	if req.Priority < 0 || req.Priority > 10 {
		req.Priority = 0
	}
	for _, f := range req.Formats {
		if !convert.Valid(f) {
			problem(w, r, 422, "invalid-format", "Unknown output format "+f, "Valid formats: raw.zst, "+strings.Join(convert.Formats, ", "))
			return
		}
	}
	sv, err := s.db.GetSpecVersion(r.Context(), p.OrgID, req.SpecVersionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Re-validate against the current catalog and org policy (invariant 1).
	doc, err := osspec.ParseDocument(sv.Spec)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := osspec.LoadDocument(doc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := specsvc.ValidateLoaded(l, s.Catalog(), s.policy(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	assetExt, debs, layouts, fe := s.resolveAssets(r, p.OrgID, v.Spec)
	if len(fe) > 0 {
		writeProblem(w, r, Problem{Type: problemBase + "spec-invalid", Title: "Spec is invalid", Status: 422, Layer: "assets", Errors: fe})
		return
	}
	out, err := specsvc.Compile(v, s.Catalog().Version, s.cfg.Platform, specsvc.Assets{Ext: assetExt, Debs: debs, Layouts: layouts})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var tarBuf bytes.Buffer
	if err := out.Tree.WriteTar(&tarBuf); err != nil {
		s.fail(w, r, err)
		return
	}
	treeDigest, _, err := s.blobs.Put(r.Context(), &tarBuf, out.Tree.Digest(), 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The platform record carries what the pipeline needs beyond the tree:
	// tenant endpoints and whether every target is a VM (Secure Boot keys are
	// then enrolled automatically on first boot).
	platform, _ := json.Marshal(struct {
		compiler.Platform
		Virtual bool `json:"virtual"`
	}{s.cfg.Platform, v.Resolution.Virtual})
	formats := req.Formats
	if len(formats) == 0 {
		formats = convert.Default(v.Resolution.Virtual, !v.Resolution.Virtual)
	}
	predicted := 20.0
	if p50, err := s.db.StageP50(r.Context()); err == nil && len(p50) > 0 {
		predicted = 0
		for _, m := range p50 {
			predicted += m
		}
	}
	b, err := s.db.CreateBuild(r.Context(), &store.Build{OrgID: p.OrgID, ProjectID: sv.ProjectID, SpecVersionID: sv.ID,
		Arch: v.Spec.Foundation.Arch[0], CatalogVersion: s.Catalog().Version, TreeDigest: treeDigest, Platform: platform,
		Priority: req.Priority, PredictedMinutes: predicted, CreatedBy: p.UserID})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.db.SetBuildFormats(r.Context(), b.ID, formats); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.cfg.EnvelopeKey != nil {
		ttl := s.cfg.EnvelopeTTL
		if ttl == 0 {
			ttl = 7 * 24 * time.Hour
		}
		now := time.Now().UTC()
		env, err := envelope.Sign(s.cfg.EnvelopeKey, envelope.Envelope{BuildID: b.ID, OrgID: b.OrgID, SpecDigest: sv.Digest,
			TreeDigest: treeDigest, CatalogVersion: b.CatalogVersion, Assets: v.Spec.AssetDigests(), Formats: formats, IssuedAt: now, ExpiresAt: now.Add(ttl)})
		if err == nil {
			err = s.db.SetEnvelope(r.Context(), b.ID, env)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "build.create", b.ID, map[string]any{"spec": sv.ID, "tree": treeDigest, "formats": formats})
	s.metrics.Counter("foundry_builds_created_total", "Builds created", 1)
	resp, _ := json.Marshal(map[string]any{"build": b, "warnings": nonNilFE(append(v.Warnings, warnStrings(out.Warnings)...))})
	if idemKey != "" {
		s.db.PutIdempotency(r.Context(), p.OrgID, idemKey, reqHash[:], 202, resp)
	}
	if s.hooks.BuildCreated != nil {
		s.hooks.BuildCreated(b.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/v1/orgs/"+p.OrgID+"/builds/"+b.ID)
	w.WriteHeader(202)
	w.Write(resp)
}

func warnStrings(ws []string) []osspec.FieldError {
	var out []osspec.FieldError
	for _, w := range ws {
		out = append(out, osspec.FieldError{Code: "CMP-W", Message: w})
	}
	return out
}

func (s *Server) listBuilds(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	cur, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		problem(w, r, 400, "bad-cursor", "Invalid cursor", "")
		return
	}
	bs, next, err := s.db.ListBuilds(r.Context(), p.OrgID, store.BuildFilter{ProjectID: r.URL.Query().Get("project"), State: r.URL.Query().Get("state")}, cur, limitParam(r, 50, 200))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if bs == nil {
		bs = []store.Build{}
	}
	writeJSON(w, 200, map[string]any{"items": bs, "nextCursor": encodeCursor(next)})
}

func (s *Server) getBuild(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	b, err := s.db.GetBuild(r.Context(), p.OrgID, r.PathValue("build"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ev, _ := s.db.BuildEvents(r.Context(), p.OrgID, b.ID)
	st, _ := s.db.StageRuns(r.Context(), b.ID)
	if st == nil {
		st = []store.StageRun{}
	}
	writeJSON(w, 200, map[string]any{"build": b, "events": ev, "stages": st})
}

func (s *Server) cancelBuild(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	b, err := s.db.RequestCancel(r.Context(), p.OrgID, r.PathValue("build"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "build.cancel", b.ID, nil)
	writeJSON(w, 200, b)
}

// artifactExt maps artifact kinds to download file name suffixes
// (<osId>_<version>_<arch><suffix>). Kinds not listed are internal.
var artifactExt = map[string]string{
	"img.xz": ".img.xz", "disk.raw.zst": ".raw.zst", "qcow2": ".qcow2", "vmdk": ".vmdk", "ova": ".ova", "iso": ".iso",
	"uki": ".efi", "usr.zst": ".usr.raw.zst", "usr-verity": ".usr-verity.raw", "usr-verity-sig": ".usr-verity-sig.raw",
	"sha256sums": ".SHA256SUMS", "sha256sums.sig": ".SHA256SUMS.sig", "sbom": ".spdx.json", "provenance": ".intoto.json",
	"scan-report": ".scan.json", "boot-log": ".boot.log", "manifest-mkosi": ".packages.json", "cidx": ".cidx", "sources": ".sources.tar",
	"pubkey-artifact": ".artifact.pub.pem", "pubkey-db": ".db.crt", "file-manifest": ".files.jsonl.gz",
}

// reportKinds are downloadable at any state (they describe, not install, the image).
var reportKinds = map[string]bool{"sbom": true, "scan-report": true, "boot-log": true, "manifest-mkosi": true, "file-manifest": true,
	"pubkey-artifact": true, "pubkey-db": true, "provenance": true}

func (s *Server) buildArtifacts(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	b, err := s.db.GetBuild(r.Context(), p.OrgID, r.PathValue("build"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	as, err := s.db.Artifacts(r.Context(), p.OrgID, b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pr, err := s.db.GetProject(r.Context(), p.OrgID, b.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	version := "0"
	if sv, err := s.db.GetSpecVersion(r.Context(), p.OrgID, b.SpecVersionID); err == nil {
		var meta struct {
			Metadata struct{ Version string } `json:"metadata"`
		}
		json.Unmarshal(sv.Spec, &meta)
		version = meta.Metadata.Version
	}
	type item struct {
		store.Artifact
		Name string `json:"name"`
		URL  string `json:"url,omitempty"`
	}
	// Images are downloadable only once signed; unsigned build outputs never are.
	released := b.State == store.StatePublished || b.State == store.StateSigned
	items := []item{}
	for _, a := range as {
		ext, public := artifactExt[a.Kind]
		if !public {
			continue
		}
		name := pr.OSID + "_" + version + ext // same names as in SHA256SUMS
		it := item{Artifact: a, Name: name}
		if reportKinds[a.Kind] || (released && a.Signed) || (released && (a.Kind == "sha256sums" || a.Kind == "usr-verity" || a.Kind == "usr.zst")) {
			it.URL = s.signer.DownloadURL(a.Digest, name, 15*time.Minute)
		}
		items = append(items, it)
	}
	writeJSON(w, 200, map[string]any{"items": items, "downloadable": released})
}

// ---------------------------------------------------------------- logs/SSE

// logHub wakes SSE streams when new log lines or state changes arrive.
type logHub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]bool
}

func newLogHub() *logHub { return &logHub{subs: map[string]map[chan struct{}]bool{}} }

func (h *logHub) sub(id string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make(chan struct{}, 1)
	if h.subs[id] == nil {
		h.subs[id] = map[chan struct{}]bool{}
	}
	h.subs[id][c] = true
	return c
}

func (h *logHub) unsub(id string, c chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs[id], c)
	if len(h.subs[id]) == 0 {
		delete(h.subs, id)
	}
}

// Notify wakes subscribers of a build (called by in-process workers).
func (s *Server) Notify(buildID string) {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	for c := range s.hub.subs[buildID] {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (s *Server) buildLogs(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	id := r.PathValue("build")
	b, err := s.db.GetBuild(r.Context(), p.OrgID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.Atoi(v)
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		lines, err := s.db.Logs(r.Context(), p.OrgID, id, after, limitParam(r, 1000, 5000))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if lines == nil {
			lines = []store.LogLine{}
		}
		writeJSON(w, 200, map[string]any{"items": lines, "state": b.State})
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		problem(w, r, 500, "no-streaming", "Streaming unsupported", "")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	wake := s.hub.sub(id)
	defer s.hub.unsub(id, wake)
	lastState := ""
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		lines, err := s.db.Logs(r.Context(), p.OrgID, id, after, 500)
		if err != nil {
			return
		}
		for _, l := range lines {
			data, _ := json.Marshal(l)
			fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", l.Seq, data)
			after = l.Seq
		}
		cur, err := s.db.GetBuild(r.Context(), p.OrgID, id)
		if err != nil {
			return
		}
		if cur.State != lastState {
			data, _ := json.Marshal(map[string]any{"state": cur.State, "tier": cur.Tier, "risk": cur.RiskScore})
			fmt.Fprintf(w, "event: state\ndata: %s\n\n", data)
			lastState = cur.State
		}
		fl.Flush()
		if len(lines) == 500 {
			continue
		}
		if store.Terminal(cur.State) || cur.State == store.StatePublished || cur.State == store.StateAwaitingReview {
			fmt.Fprintf(w, "event: end\ndata: {}\n\n")
			fl.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-time.After(time.Second):
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
		}
	}
}

// resolveAssets checks that every referenced upload exists in the org, passed
// validation and has the right kind, and returns compiler inputs derived
// from validation reports (file extensions, .deb package names).
func (s *Server) resolveAssets(r *http.Request, orgID string, spec *osspec.Spec) (map[string]string, map[string]string, map[string]*osspec.LayoutDoc, []osspec.FieldError) {
	ext, debs, layouts := map[string]string{}, map[string]string{}, map[string]*osspec.LayoutDoc{}
	var fe []osspec.FieldError
	want := map[string]string{} // digest → required kind ("" = any)
	ptr := map[string]string{}
	if spec.Look != nil && spec.Look.Layout != nil && spec.Look.Layout.Custom != "" {
		want[spec.Look.Layout.Custom], ptr[spec.Look.Layout.Custom] = "layout", "/look/layout/custom"
	}
	if spec.Apps != nil {
		for i, d := range spec.Apps.Custom {
			want[d], ptr[d] = "deb", fmt.Sprintf("/apps/custom/%d", i)
		}
	}
	if e := spec.Expert; e != nil {
		for i, d := range e.Overlays {
			want[d], ptr[d] = "overlay", fmt.Sprintf("/expert/overlays/%d", i)
		}
		for i, d := range e.PostinstScripts {
			want[d], ptr[d] = "script", fmt.Sprintf("/expert/postinstScripts/%d", i)
		}
		for i, d := range e.MkosiDropins {
			want[d], ptr[d] = "mkosi-dropin", fmt.Sprintf("/expert/mkosiDropins/%d", i)
		}
		for i, rp := range e.ExtraRepos {
			want[rp.KeyAsset], ptr[rp.KeyAsset] = "gpg-key", fmt.Sprintf("/expert/extraRepos/%d/keyAsset", i)
		}
	}
	reports := map[string]upload.Report{}
	for _, d := range spec.AssetDigests() {
		a, err := s.db.GetAsset(r.Context(), orgID, d)
		if err != nil || a.Status != "valid" {
			fe = append(fe, osspec.FieldError{Pointer: ptr[d], Code: "AST-404", Message: "Uploaded file " + d + " is missing or failed validation."})
			continue
		}
		if k := want[d]; k != "" && a.Kind != k {
			fe = append(fe, osspec.FieldError{Pointer: ptr[d], Code: "AST-409", Message: fmt.Sprintf("Uploaded file %s is a %s, expected a %s.", d, a.Kind, k)})
			continue
		}
		ext[d] = MediaExt[a.MediaType]
		var rep upload.Report
		json.Unmarshal(a.Validation, &rep)
		reports[d] = rep
		if a.Kind == "deb" {
			debs[d] = rep.Package
		}
		if a.Kind == "layout" {
			doc, err := s.layoutDoc(r, d)
			if err != nil {
				fe = append(fe, osspec.FieldError{Pointer: ptr[d], Code: "AST-LAYOUT", Message: err.Error()})
				continue
			}
			layouts[d] = doc
		}
	}
	if e := spec.Expert; e != nil {
		for i, rp := range e.ExtraRepos {
			if rep, ok := reports[rp.KeyAsset]; ok && !strings.EqualFold(rep.Fingerprint, rp.KeyFingerprint) {
				fe = append(fe, osspec.FieldError{Pointer: fmt.Sprintf("/expert/extraRepos/%d/keyFingerprint", i), Code: "AST-KEY",
					Message: "The uploaded key's fingerprint (" + rep.Fingerprint + ") does not match keyFingerprint."})
			}
		}
	}
	return ext, debs, layouts, fe
}

func (s *Server) layoutDoc(r *http.Request, digest string) (*osspec.LayoutDoc, error) {
	f, _, err := s.blobs.Open(r.Context(), digest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil {
		return nil, err
	}
	return osspec.ParseLayout(b)
}
