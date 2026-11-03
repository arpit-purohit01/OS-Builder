// Package catalog loads the option catalog, compiles its constraints to CNF,
// solves them with a small DPLL/CDCL-lite SAT solver and explains conflicts
// with a minimal unsatisfiable subset (MUS) of the user's selections.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"foundry.dev/foundry/internal/osspec"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"foundry.dev/foundry/internal/jcs"
	"gopkg.in/yaml.v3"
)

// Option is one selectable catalog entry.
type Option struct {
	ID         string              `yaml:"id" json:"id"`
	Kind       string              `yaml:"kind" json:"kind"`
	Label      string              `yaml:"label" json:"label"`
	Bases      []string            `yaml:"bases,omitempty" json:"bases,omitempty"`
	Arch       []string            `yaml:"arch,omitempty" json:"arch,omitempty"`
	Requires   []string            `yaml:"requires,omitempty" json:"requires,omitempty"`
	Conflicts  []string            `yaml:"conflicts,omitempty" json:"conflicts,omitempty"`
	Provides   []string            `yaml:"provides,omitempty" json:"provides,omitempty"`
	OneOf      string              `yaml:"oneOf,omitempty" json:"oneOf,omitempty"`
	Packages   map[string][]string `yaml:"packages,omitempty" json:"packages,omitempty"`
	VMPackages map[string][]string `yaml:"vmPackages,omitempty" json:"vmPackages,omitempty"`
	Components []string            `yaml:"components,omitempty" json:"components,omitempty"`
	Virtual    bool                `yaml:"virtual,omitempty" json:"virtual,omitempty"`
	Risk       *Risk               `yaml:"risk,omitempty" json:"risk,omitempty"`
	UI         *UI                 `yaml:"ui,omitempty" json:"ui,omitempty"`
	HCL        *HCL                `yaml:"hcl,omitempty" json:"hcl,omitempty"`
}

// HCL is the hardware compatibility record of a device option: which bases
// were verified on it (lab and pilot devices).
type HCL struct {
	Status string   `yaml:"status" json:"status"` // certified | tested | community
	Tested []string `yaml:"tested" json:"tested"` // base ids
	Notes  string   `yaml:"notes,omitempty" json:"notes,omitempty"`
}

type Risk struct {
	Flags []string `yaml:"flags" json:"flags"`
}

type UI struct {
	Icon   string   `yaml:"icon,omitempty" json:"icon,omitempty"`
	Levels []string `yaml:"levels,omitempty" json:"levels,omitempty"`
	Help   string   `yaml:"help,omitempty" json:"help,omitempty"`
}

// Catalog is an immutable, versioned set of options.
type Catalog struct {
	Version string              `json:"version"` // sha256 of canonical option list
	Options []Option            `json:"options"`
	byID    map[string]*Option  `json:"-"`
	virtual map[string][]string // virtual id -> providers
}

// Bases known to the platform.
var Bases = []string{"ubuntu-24.04", "ubuntu-26.04", "debian-13", "fedora"}

type file struct {
	Options []Option `yaml:"options"`
}

// LoadDir loads every *.yaml in dir.
func LoadDir(dir string) (*Catalog, error) {
	return LoadFS(os.DirFS(dir))
}

// LoadFS loads every *.yaml at the root of fsys.
func LoadFS(fsys fs.FS) (*Catalog, error) {
	names, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var all []Option
	for _, n := range names {
		data, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		var f file
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", filepath.Base(n), err)
		}
		all = append(all, f.Options...)
	}
	return New(all)
}

// New builds and checks a catalog.
func New(opts []Option) (*Catalog, error) {
	sort.Slice(opts, func(i, j int) bool { return opts[i].ID < opts[j].ID })
	c := &Catalog{Options: opts, byID: map[string]*Option{}, virtual: map[string][]string{}}
	for i := range opts {
		o := &opts[i]
		if o.ID == "" || o.Label == "" || o.Kind == "" {
			return nil, fmt.Errorf("catalog: option %q missing id/label/kind", o.ID)
		}
		if strings.HasPrefix(o.ID, "#") {
			return nil, fmt.Errorf("catalog: id %q uses reserved prefix", o.ID)
		}
		if _, dup := c.byID[o.ID]; dup {
			return nil, fmt.Errorf("catalog: duplicate id %q", o.ID)
		}
		for _, b := range o.Bases {
			if !contains(Bases, b) {
				return nil, fmt.Errorf("catalog: %s: unknown base %q", o.ID, b)
			}
		}
		for b := range o.Packages {
			if b != "default" && !contains(Bases, b) {
				return nil, fmt.Errorf("catalog: %s: unknown package base %q", o.ID, b)
			}
		}
		c.byID[o.ID] = o
	}
	for i := range opts {
		for _, p := range opts[i].Provides {
			if _, real := c.byID[p]; real {
				return nil, fmt.Errorf("catalog: %s provides %q which is a real option", opts[i].ID, p)
			}
			c.virtual[p] = append(c.virtual[p], opts[i].ID)
		}
	}
	for i := range opts {
		o := &opts[i]
		for _, ref := range append(append([]string{}, o.Requires...), o.Conflicts...) {
			if _, ok := c.byID[ref]; !ok {
				if _, ok := c.virtual[ref]; !ok {
					return nil, fmt.Errorf("catalog: %s references unknown %q", o.ID, ref)
				}
			}
		}
	}
	canon, err := jcs.Marshal(opts)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(canon)
	c.Version = hex.EncodeToString(h[:])[:16]
	return c, nil
}

// Get returns an option by id.
func (c *Catalog) Get(id string) (*Option, bool) {
	o, ok := c.byID[id]
	return o, ok
}

// Available reports whether option o can be used on base/arch.
func (o *Option) Available(base, arch string) bool {
	if len(o.Bases) > 0 && !contains(o.Bases, base) {
		return false
	}
	if len(o.Arch) > 0 && !contains(o.Arch, arch) {
		return false
	}
	if len(o.Packages) > 0 && o.Packages["default"] == nil && o.Packages[base] == nil {
		return false
	}
	return true
}

// PackagesFor returns the package list for base (falls back to "default").
func (o *Option) PackagesFor(base string, vm bool) []string {
	src := o.Packages
	if vm && o.VMPackages != nil {
		src = o.VMPackages
	}
	if p, ok := src[base]; ok {
		return p
	}
	return src["default"]
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// HCLWarnings flags hardware targets that were not verified with the base.
func (c *Catalog) HCLWarnings(s *osspec.Spec) []osspec.FieldError {
	var out []osspec.FieldError
	if s.Hardware == nil {
		return nil
	}
	for i, t := range s.Hardware.Targets {
		o, ok := c.byID["hw."+t]
		if !ok || o.HCL == nil {
			continue
		}
		tested := false
		for _, b := range o.HCL.Tested {
			if b == s.Foundation.Base {
				tested = true
			}
		}
		if !tested {
			out = append(out, osspec.FieldError{Pointer: fmt.Sprintf("/hardware/targets/%d", i), Code: "HCL-W01",
				Message: fmt.Sprintf("%s has been tested with %s, not with %s. It will probably work; check it on one device before rolling out.", o.Label, strings.Join(o.HCL.Tested, ", "), s.Foundation.Base)})
		}
	}
	return out
}
