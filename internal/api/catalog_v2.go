package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/ids"
	"foundry.dev/foundry/internal/store"
	"gopkg.in/yaml.v3"
)

// getCatalogVersions returns all catalog versions.
func (s *Server) getCatalogVersions(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	versions, err := s.db.ListCatalogVersions(r.Context(), org)
	if err != nil {
		problem(w, r, 500, "internal", "Failed to list catalog versions", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"versions": versions})
}

// getCatalogVersion returns a specific catalog version with options.
func (s *Server) getCatalogVersion(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	versionID := r.PathValue("version")

	version, err := s.db.GetCatalogVersion(r.Context(), org, versionID)
	if err != nil {
		problem(w, r, 404, "not-found", "Catalog version not found", err.Error())
		return
	}

	cat := s.Catalog()
	base := r.URL.Query().Get("base")
	if base == "" {
		base = "ubuntu-24.04"
	}
	arch := r.URL.Query().Get("arch")
	if arch == "" {
		arch = "x86_64"
	}

	// Filter options by base/arch availability
	type opt struct {
		ID        string   `json:"id"`
		Kind      string   `json:"kind"`
		Label     string   `json:"label"`
		OneOf     string   `json:"oneOf,omitempty"`
		Available bool     `json:"available"`
		Levels    []string `json:"levels,omitempty"`
		Help      string   `json:"help,omitempty"`
		Icon      string   `json:"icon,omitempty"`
		Requires  []string `json:"requires,omitempty"`
		Conflicts []string `json:"conflicts,omitempty"`
		Risk      []string `json:"risk,omitempty"`
		HCL       any      `json:"hcl,omitempty"`
	}

	out := make([]any, 0, len(cat.Options))
	for i := range cat.Options {
		o := &cat.Options[i]
		if o.Available(base, arch) {
			x := map[string]any{
				"ID":        o.ID,
				"Kind":      o.Kind,
				"Label":     o.Label,
				"OneOf":     o.OneOf,
				"Available": true,
				"Requires":  o.Requires,
				"Conflicts": o.Conflicts,
			}
			if o.UI != nil {
				x["Levels"], x["Help"], x["Icon"] = o.UI.Levels, o.UI.Help, o.UI.Icon
			}
			if o.Risk != nil {
				x["Risk"] = o.Risk.Flags
			}
			if o.HCL != nil {
				x["HCL"] = o.HCL
			}
			out = append(out, x)
		}
	}

	body, _ := json.Marshal(map[string]any{
		"version":   version,
		"base":      base,
		"arch":      arch,
		"options":   out,
		"catalogID": version.CatalogID,
	})
	serveCached(w, r, body, false)
}

// createCatalogVersion creates a new catalog version from uploaded YAML files.
func (s *Server) createCatalogVersion(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")

	var req struct {
		YAMLFiles map[string]string `json:"yaml_files"` // filename -> content
		Base      string            `json:"base"`
		Arch      string            `json:"arch"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Base == "" {
		req.Base = "ubuntu-24.04"
	}
	if req.Arch == "" {
		req.Arch = "x86_64"
	}

	// Parse YAML files into options
	var allOptions []catalog.Option
	for filename, content := range req.YAMLFiles {
		var f struct {
			Options []catalog.Option `yaml:"options"`
		}
		dec := yaml.NewDecoder(strings.NewReader(content))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil {
			problem(w, r, 400, "bad-request", "Invalid YAML in "+filename, err.Error())
			return
		}
		allOptions = append(allOptions, f.Options...)
	}

	// Create catalog
	cat, err := catalog.New(allOptions)
	if err != nil {
		problem(w, r, 400, "bad-request", "Invalid catalog", err.Error())
		return
	}

	// Compile CNF for base/arch
	cnf := cat.Compile(req.Base, req.Arch)
	cnfBody, _ := json.Marshal(cnf)
	cnfDigest := sha256.Sum256(cnfBody)

	// Store catalog version
	version := &store.CatalogVersion{
		ID:           ids.New("cat"),
		CatalogID:    cat.Version,
		CNFDigest:    cnfDigest[:],
		Base:         req.Base,
		Arch:         req.Arch,
		CreatedAt:    time.Now(),
		IsDefault:    false,
		OptionsCount: len(cat.Options),
	}

	if err := s.db.CreateCatalogVersion(r.Context(), org, version); err != nil {
		problem(w, r, 500, "internal", "Failed to create catalog version", err.Error())
		return
	}

	// Set as default if first version
	versions, _ := s.db.ListCatalogVersions(r.Context(), org)
	if len(versions) == 1 {
		s.db.SetDefaultCatalogVersion(r.Context(), org, version.ID)
		version.IsDefault = true
	}

	// Hot-swap catalog if this is the default
	if version.IsDefault {
		s.SetCatalog(cat)
	}

	writeJSON(w, 201, version)
}

// setDefaultCatalogVersion sets a catalog version as default.
func (s *Server) setDefaultCatalogVersion(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	versionID := r.PathValue("version")

	if err := s.db.SetDefaultCatalogVersion(r.Context(), org, versionID); err != nil {
		problem(w, r, 500, "internal", "Failed to set default catalog version", err.Error())
		return
	}

	// Reload catalog if this is the default
	v, _ := s.db.GetCatalogVersion(r.Context(), org, versionID)
	cat, _ := catalog.LoadDir("catalog")
	if v.CatalogID == cat.Version {
		s.SetCatalog(cat)
	}

	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// getCNFForVersion returns the CNF for a specific catalog version.
func (s *Server) getCNFForVersion(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	versionID := r.PathValue("version")

	_, err := s.db.GetCatalogVersion(r.Context(), org, versionID)
	if err != nil {
		problem(w, r, 404, "not-found", "Catalog version not found", err.Error())
		return
	}

	base := r.URL.Query().Get("base")
	if base == "" {
		base = "ubuntu-24.04"
	}
	arch := r.URL.Query().Get("arch")
	if arch == "" {
		arch = "x86_64"
	}

	// For now, use current catalog. In production, we'd load the specific version's catalog.
	cat := s.Catalog()
	cnf := cat.Compile(base, arch)

	body, _ := json.Marshal(cnf)
	serveCached(w, r, body, true)
}
