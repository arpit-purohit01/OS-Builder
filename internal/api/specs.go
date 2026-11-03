package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"foundry.dev/foundry/internal/jsonpatch"
	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/specsvc"
	"foundry.dev/foundry/internal/store"
	"gopkg.in/yaml.v3"
)

var osIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{2,39}$`)

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.db.ListProjects(r.Context(), getPrincipal(r.Context()).OrgID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ps == nil {
		ps = []store.Project{}
	}
	writeJSON(w, 200, map[string]any{"items": ps})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	var req struct {
		Name string `json:"name"`
		OSID string `json:"osId"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 || !osIDRe.MatchString(req.OSID) {
		problem(w, r, 422, "invalid", "Enter a name and an OS id (lowercase, 3–40 chars, starts with a letter)", "")
		return
	}
	pr, err := s.db.CreateProject(r.Context(), p.OrgID, req.Name, req.OSID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "project.create", pr.ID, nil)
	writeJSON(w, 201, pr)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	pr, err := s.db.GetProject(r.Context(), p.OrgID, r.PathValue("project"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	latest, _, _ := s.db.ListSpecVersions(r.Context(), p.OrgID, pr.ID, nil, 1)
	resp := map[string]any{"project": pr}
	if len(latest) > 0 {
		resp["latestSpec"] = latest[0]
	}
	writeJSON(w, 200, resp)
}

func (s *Server) listSpecs(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	cur, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		problem(w, r, 400, "bad-cursor", "Invalid cursor", "")
		return
	}
	vs, next, err := s.db.ListSpecVersions(r.Context(), p.OrgID, r.PathValue("project"), cur, limitParam(r, 50, 200))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if vs == nil {
		vs = []store.SpecVersion{}
	}
	writeJSON(w, 200, map[string]any{"items": vs, "nextCursor": encodeCursor(next)})
}

func (s *Server) policy(r *http.Request) specsvc.Policy {
	pol := specsvc.DefaultPolicy()
	if o, err := s.db.GetOrg(r.Context(), getPrincipal(r.Context()).OrgID); err == nil {
		pol.ExpertAllowed = o.ExpertAllowed
	}
	return pol
}

// readSpecBody returns either a raw spec document (YAML/JSON) or a patch request.
func readSpecBody(r *http.Request) (raw []byte, parent string, patch jsonpatch.Patch, err error) {
	body, err := readAllLimited(r.Body, osspec.MaxSpecBytes)
	if err != nil {
		return nil, "", nil, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var env struct {
			Parent string          `json:"parent"`
			Patch  jsonpatch.Patch `json:"patch"`
			Spec   json.RawMessage `json:"spec"`
		}
		if json.Unmarshal(trimmed, &env) == nil && (env.Patch != nil || env.Spec != nil) {
			if env.Spec != nil {
				return env.Spec, env.Parent, nil, nil
			}
			return nil, env.Parent, env.Patch, nil
		}
	}
	return body, "", nil, nil
}

// loadSubmitted resolves the body into a validated spec.
func (s *Server) loadSubmitted(r *http.Request) (*specsvc.Validated, []byte, *store.SpecVersion, error) {
	p := getPrincipal(r.Context())
	raw, parentID, patch, err := readSpecBody(r)
	if err != nil {
		return nil, nil, nil, &osspec.ValidationError{Layer: "parse", Errors: []osspec.FieldError{{Code: "PARSE-001", Message: err.Error()}}}
	}
	var parent *store.SpecVersion
	if parentID != "" {
		parent, err = s.db.GetSpecVersion(r.Context(), p.OrgID, parentID)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	var doc map[string]any
	if patch != nil {
		if parent == nil {
			return nil, nil, nil, &osspec.ValidationError{Layer: "parse", Errors: []osspec.FieldError{{Code: "PATCH-001", Message: "a patch needs a parent spec version"}}}
		}
		var base any
		json.Unmarshal(parent.Spec, &base)
		out, err := jsonpatch.Apply(base, patch)
		if err != nil {
			return nil, nil, nil, &osspec.ValidationError{Layer: "parse", Errors: []osspec.FieldError{{Code: "PATCH-002", Message: err.Error()}}}
		}
		m, ok := out.(map[string]any)
		if !ok {
			return nil, nil, nil, &osspec.ValidationError{Layer: "parse", Errors: []osspec.FieldError{{Code: "PATCH-003", Message: "patched document is not an object"}}}
		}
		b, _ := json.Marshal(m)
		raw = b
		doc, err = osspec.ParseDocument(b)
		if err != nil {
			return nil, nil, nil, err
		}
	} else {
		doc, err = osspec.ParseDocument(raw)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	l, err := osspec.LoadDocument(doc)
	if err != nil {
		return nil, nil, nil, err
	}
	v, err := specsvc.ValidateLoaded(l, s.Catalog(), s.policy(r))
	if err != nil {
		return nil, nil, nil, err
	}
	return v, raw, parent, nil
}

func (s *Server) validateSpec(w http.ResponseWriter, r *http.Request) {
	v, _, _, err := s.loadSubmitted(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"valid": true, "digest": v.Digest, "warnings": nonNilFE(v.Warnings),
		"selected": v.Resolution.Selected, "implied": v.Resolution.Implied, "packages": len(v.Resolution.Packages)})
}

func nonNilFE(x []osspec.FieldError) []osspec.FieldError {
	if x == nil {
		return []osspec.FieldError{}
	}
	return x
}

func (s *Server) createSpec(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	pr, err := s.db.GetProject(r.Context(), p.OrgID, r.PathValue("project"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, raw, parent, err := s.loadSubmitted(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v.Spec.Branding.OSID != pr.OSID {
		writeProblem(w, r, Problem{Type: problemBase + "spec-invalid", Title: "Spec is invalid", Status: 422, Layer: "project",
			Errors: []osspec.FieldError{{Pointer: "/branding/osId", Code: "PRJ-001", Message: "The OS id must match the project's OS id (" + pr.OSID + ")."}}})
		return
	}
	if parent == nil { // timeline: parent defaults to the latest version
		if latest, _, err := s.db.ListSpecVersions(r.Context(), p.OrgID, pr.ID, nil, 1); err == nil && len(latest) > 0 {
			parent = &latest[0]
		}
	}
	var patch json.RawMessage
	var parentID *string
	if parent != nil {
		var a, b any
		json.Unmarshal(parent.Spec, &a)
		json.Unmarshal(v.Canonical, &b)
		patch, _ = json.Marshal(jsonpatch.Diff(a, b))
		parentID = &parent.ID
	}
	warns, _ := json.Marshal(nonNilFE(v.Warnings))
	sv, created, err := s.db.CreateSpecVersion(r.Context(), &store.SpecVersion{
		OrgID: p.OrgID, ProjectID: pr.ID, Digest: v.Digest, APIVersion: v.Spec.APIVersion, Spec: v.Canonical,
		Source: string(raw), ParentID: parentID, Patch: patch, CatalogVersion: s.Catalog().Version,
		HasExpert: v.Spec.HasExpert(), Warnings: warns, CreatedBy: p.UserID,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := 200
	if created {
		status = 201
		s.audit(r.Context(), p.OrgID, p.UserID, "spec.create", sv.ID, map[string]string{"digest": v.Digest})
	}
	writeJSON(w, status, map[string]any{"specVersion": sv, "created": created, "warnings": nonNilFE(v.Warnings),
		"implied": v.Resolution.Implied, "packages": len(v.Resolution.Packages)})
}

func (s *Server) getSpec(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	sv, err := s.db.GetSpecVersion(r.Context(), p.OrgID, r.PathValue("spec"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if r.URL.Query().Get("format") == "yaml" {
		var doc yaml.Node
		if err := yaml.Unmarshal(sv.Spec, &doc); err != nil { // JSON is valid YAML
			s.fail(w, r, err)
			return
		}
		setBlockStyle(&doc)
		out, _ := yaml.Marshal(&doc)
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(out)
		return
	}
	writeJSON(w, 200, sv)
}

func setBlockStyle(n *yaml.Node) {
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		n.Style = 0
	}
	if n.Kind == yaml.ScalarNode && n.Style == yaml.DoubleQuotedStyle {
		n.Style = 0
	}
	for _, c := range n.Content {
		setBlockStyle(c)
	}
}

func (s *Server) diffSpecs(w http.ResponseWriter, r *http.Request) {
	p := getPrincipal(r.Context())
	a, err := s.db.GetSpecVersion(r.Context(), p.OrgID, r.PathValue("spec"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b, err := s.db.GetSpecVersion(r.Context(), p.OrgID, r.PathValue("other"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var da, db any
	json.Unmarshal(a.Spec, &da)
	json.Unmarshal(b.Spec, &db)
	patch := jsonpatch.Diff(da, db)
	if patch == nil {
		patch = jsonpatch.Patch{}
	}
	writeJSON(w, 200, map[string]any{"from": a.ID, "to": b.ID, "patch": patch})
}
