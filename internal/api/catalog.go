package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/osspec"
)

func etagFor(b []byte) string {
	h := sha256.Sum256(b)
	return `"` + hex.EncodeToString(h[:12]) + `"`
}

func serveCached(w http.ResponseWriter, r *http.Request, b []byte, immutable bool) {
	etag := etagFor(b)
	w.Header().Set("ETag", etag)
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// getCatalog returns options with UI metadata plus availability for base/arch.
func (s *Server) getCatalog(w http.ResponseWriter, r *http.Request) {
	cat := s.Catalog()
	base := r.URL.Query().Get("base")
	if base == "" {
		base = "ubuntu-24.04"
	}
	arch := r.URL.Query().Get("arch")
	if arch == "" {
		arch = "x86_64"
	}
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
	out := make([]opt, 0, len(cat.Options))
	for i := range cat.Options {
		o := &cat.Options[i]
		x := opt{ID: o.ID, Kind: o.Kind, Label: o.Label, OneOf: o.OneOf, Available: o.Available(base, arch), Requires: o.Requires, Conflicts: o.Conflicts}
		if o.UI != nil {
			x.Levels, x.Help, x.Icon = o.UI.Levels, o.UI.Help, o.UI.Icon
		}
		if o.Risk != nil {
			x.Risk = o.Risk.Flags
		}
		if o.HCL != nil {
			x.HCL = o.HCL
		}
		out = append(out, x)
	}
	var bases []map[string]any
	for _, id := range compiler.SupportedBases() {
		b, _ := compiler.LookupBase(id)
		bases = append(bases, map[string]any{"id": id, "tested": b.Tested})
	}
	body, _ := json.Marshal(map[string]any{"version": cat.Version, "base": base, "arch": arch, "options": out, "bases": bases})
	serveCached(w, r, body, false)
}

func (s *Server) getCNF(w http.ResponseWriter, r *http.Request) {
	cat := s.Catalog()
	if r.PathValue("version") != cat.Version {
		problem(w, r, 404, "not-found", "Unknown catalog version", "Only the current version "+cat.Version+" is served.")
		return
	}
	base, arch := r.URL.Query().Get("base"), r.URL.Query().Get("arch")
	if base == "" {
		base = "ubuntu-24.04"
	}
	if arch == "" {
		arch = "x86_64"
	}
	body, _ := json.Marshal(cat.Compile(base, arch))
	serveCached(w, r, body, true)
}

func (s *Server) getSchema(w http.ResponseWriter, r *http.Request) {
	serveCached(w, r, osspec.SchemaJSON(), false)
}
