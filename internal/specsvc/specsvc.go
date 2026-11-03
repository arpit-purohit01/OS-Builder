// Package specsvc runs the full validation pipeline for a spec document:
// layer 1 schema, 2 semantic (osspec), 3 catalog SAT (catalog), 4 org policy.
// It is the only entry point the API and builder use, so every compiled spec
// has passed all four layers (invariant 1 in CLAUDE.md).
package specsvc

import (
	"fmt"
	"foundry.dev/foundry/internal/osspec"

	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/compiler"
)

// Policy is the per-organization limit set (layer 4).
type Policy struct {
	ExpertAllowed bool     `json:"expertAllowed"`
	AllowedBases  []string `json:"allowedBases,omitempty"` // empty = all compilable bases
	MaxAssetCount int      `json:"maxAssetCount,omitempty"`
}

// DefaultPolicy for new organizations.
func DefaultPolicy() Policy { return Policy{ExpertAllowed: false, MaxAssetCount: 50} }

// Validated is a spec that passed all layers.
type Validated struct {
	*osspec.Loaded
	Resolution *catalog.Resolution
	Warnings   []osspec.FieldError
}

// Validate parses and validates a spec document (YAML or JSON).
func Validate(data []byte, cat *catalog.Catalog, pol Policy) (*Validated, error) {
	l, err := osspec.Load(data)
	if err != nil {
		return nil, err
	}
	return ValidateLoaded(l, cat, pol)
}

// ValidateLoaded runs layers 3–4 on an already loaded spec.
func ValidateLoaded(l *osspec.Loaded, cat *catalog.Catalog, pol Policy) (*Validated, error) {
	r := cat.Check(l.Spec)
	if !r.OK {
		return nil, &osspec.ValidationError{Layer: "catalog", Errors: r.Errors}
	}
	var perr []osspec.FieldError
	var warn []osspec.FieldError
	base := l.Spec.Foundation.Base
	if _, err := compiler.LookupBase(base); err != nil {
		perr = append(perr, osspec.FieldError{Pointer: "/foundation/base", Code: "POL-001", Message: fmt.Sprintf("%s images cannot be built yet.", base)})
	} else if b, _ := compiler.LookupBase(base); !b.Tested {
		warn = append(warn, osspec.FieldError{Pointer: "/foundation/base", Code: "POL-W01", Message: fmt.Sprintf("%s is supported but not yet boot-tested on this platform.", base)})
	}
	if len(pol.AllowedBases) > 0 && !contains(pol.AllowedBases, base) {
		perr = append(perr, osspec.FieldError{Pointer: "/foundation/base", Code: "POL-003", Message: "Your plan does not include this base system."})
	}
	if l.Spec.HasExpert() && !pol.ExpertAllowed {
		perr = append(perr, osspec.FieldError{Pointer: "/expert", Code: "POL-002", Message: "Expert options (custom packages, scripts, kernel parameters) are not enabled for your organization."})
	}
	if pol.MaxAssetCount > 0 && len(l.Spec.AssetDigests()) > pol.MaxAssetCount {
		perr = append(perr, osspec.FieldError{Pointer: "", Code: "POL-004", Message: "Too many uploaded files referenced."})
	}
	if len(perr) > 0 {
		return nil, &osspec.ValidationError{Layer: "policy", Errors: perr}
	}
	warn = append(warn, cat.HCLWarnings(l.Spec)...)
	return &Validated{Loaded: l, Resolution: r, Warnings: append(append([]osspec.FieldError{}, l.Warnings...), warn...)}, nil
}

// Compile compiles a validated spec.
// Assets carries what the compiler needs to know about uploaded files.
type Assets struct {
	Ext     map[string]string            // digest → file extension
	Debs    map[string]string            // .deb digest → package name
	Layouts map[string]*osspec.LayoutDoc // layout digest → document
}

func Compile(v *Validated, catVersion string, p compiler.Platform, a Assets) (*compiler.Output, error) {
	return compiler.Compile(compiler.Input{
		Spec: v.Spec, SpecDigest: v.Digest, Resolution: v.Resolution, Catalog: catVersion, Platform: p,
		AssetExt: a.Ext, DebPackages: a.Debs, Layouts: a.Layouts,
	})
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
