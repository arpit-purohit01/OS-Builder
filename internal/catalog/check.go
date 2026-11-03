package catalog

import (
	"fmt"
	"sort"
	"strings"

	"foundry.dev/foundry/internal/osspec"
)

// Selection is one user choice derived from the spec, with the JSON pointer
// that produced it (for error messages and UI fixes).
type Selection struct {
	ID      string
	Pointer string
}

// Selections derives catalog option ids from a defaulted spec.
// Order matters: earlier selections are treated as "older" when computing
// conflict explanations (later choices are blamed first).
func Selections(s *osspec.Spec) []Selection {
	var out []Selection
	add := func(id, ptr string) { out = append(out, Selection{ID: id, Pointer: ptr}) }
	add("profile."+s.Foundation.Profile, "/foundation/profile")
	add("kernel."+s.Kernel.Flavor, "/kernel/flavor")
	add("security.level."+s.Security.Level, "/security/level")
	add("disk."+strings.ReplaceAll(s.Security.DiskEncryption, "+", "-"), "/security/diskEncryption")
	add("secureboot."+s.Security.SecureBoot, "/security/secureBoot")
	add("usb."+s.Security.USBStorage, "/security/usbStorage")
	add("access."+s.Access.RootModel, "/access/rootModel")
	add("memory."+s.Memory.Profile, "/memory/profile")
	add("identity."+s.Ecosystem.Identity.Provider, "/ecosystem/identity/provider")
	add("ecosystem.sync."+s.Ecosystem.Sync.Provider, "/ecosystem/sync/provider")
	if s.Ecosystem.AppStore.Enabled {
		add("ecosystem.appstore", "/ecosystem/appStore/enabled")
	}
	if s.Ecosystem.Handoff.Enabled {
		add("ecosystem.handoff", "/ecosystem/handoff/enabled")
		switch s.Foundation.Profile {
		case "desktop-kde":
			add("ecosystem.handoff.kde", "/ecosystem/handoff/enabled")
		case "desktop-gnome":
			add("ecosystem.handoff.gnome", "/ecosystem/handoff/enabled")
		}
	}
	if s.Ecosystem.Fleet.Enroll {
		add("ecosystem.fleet", "/ecosystem/fleet/enroll")
	}
	l := s.Look
	if l.Layout.Custom != "" {
		add("layout.custom", "/look/layout/custom")
	} else {
		add("layout."+l.Layout.Preset, "/look/layout/preset")
	}
	if l.Theme.Font != "" {
		add("font."+l.Theme.Font, "/look/theme/font")
	}
	if l.Theme.IconTheme != "" {
		add("icons."+l.Theme.IconTheme, "/look/theme/iconTheme")
	}
	if l.Wallpaper != nil && l.Wallpaper.Catalog != "" {
		add("wallpaper."+l.Wallpaper.Catalog, "/look/wallpaper/catalog")
	}
	for i, a := range s.Apps.Preinstalled {
		if strings.HasPrefix(a, "catalog:") {
			a = strings.TrimPrefix(a, "catalog:")
		}
		add(a, fmt.Sprintf("/apps/preinstalled/%d", i))
	}
	for i, a := range s.Apps.System {
		add(a, fmt.Sprintf("/apps/system/%d", i))
	}
	if s.Security.Firewall != nil {
		for i, a := range s.Security.Firewall.Allow {
			add("fw.allow."+a, fmt.Sprintf("/security/firewall/allow/%d", i))
		}
	}
	for i, h := range s.Hardware.Targets {
		add("hw."+h, fmt.Sprintf("/hardware/targets/%d", i))
	}
	for i, h := range s.Hardware.ExtraFirmware {
		add(h, fmt.Sprintf("/hardware/extraFirmware/%d", i))
	}
	return out
}

// Resolution is the outcome of catalog validation (layer 3).
type Resolution struct {
	OK         bool
	Errors     []osspec.FieldError
	Selected   []string // user selections ∪ implied, real options only, sorted
	Implied    []string // implied but not explicitly selected
	Packages   []string // sorted, deduped package list for the base
	Components []string // Foundry-provided components
	Virtual    bool     // all hardware targets are virtual machines
	Risk       []string // risk flags of selected options
}

// Check validates a defaulted spec against the catalog for its base/arch.
func (c *Catalog) Check(s *osspec.Spec) *Resolution {
	base, arch := s.Foundation.Base, s.Foundation.Arch[0]
	sels := Selections(s)
	r := &Resolution{}
	f := c.Compile(base, arch)
	var lits []Lit
	ptrOf := map[Lit]string{}
	for _, sel := range sels {
		o, ok := c.byID[sel.ID]
		if !ok {
			r.Errors = append(r.Errors, osspec.FieldError{Pointer: sel.Pointer, Code: "CAT-404", Message: fmt.Sprintf("%q is not in the catalog.", sel.ID)})
			continue
		}
		if !o.Available(base, arch) {
			r.Errors = append(r.Errors, osspec.FieldError{Pointer: sel.Pointer, Code: "CAT-410", Message: fmt.Sprintf("%s is not available for %s on %s.", o.Label, base, arch)})
			continue
		}
		v, _ := f.Var(sel.ID)
		if _, dup := ptrOf[v]; !dup {
			lits = append(lits, v)
			ptrOf[v] = sel.Pointer
		}
	}
	if len(r.Errors) > 0 {
		return r
	}
	res := Solve(f.CNF, lits)
	if !res.SAT {
		core := MUS(f.CNF, lits)
		var labels []string
		for _, l := range core {
			o := c.byID[f.Vars[l-1]]
			labels = append(labels, o.Label)
		}
		msg := "These choices cannot be combined: " + strings.Join(labels, ", ") + ". Change one of them."
		for _, l := range core {
			r.Errors = append(r.Errors, osspec.FieldError{Pointer: ptrOf[l], Code: "CAT-409", Message: msg})
		}
		return r
	}
	selected := map[string]bool{}
	for _, l := range lits {
		selected[f.Vars[l-1]] = true
	}
	all := map[string]bool{}
	for _, l := range res.Implied {
		if l <= 0 {
			continue
		}
		name := f.Vars[l-1]
		if _, real := c.byID[name]; real {
			all[name] = true
			if !selected[name] {
				r.Implied = append(r.Implied, name)
			}
		}
	}
	for n := range all {
		r.Selected = append(r.Selected, n)
	}
	sort.Strings(r.Selected)
	sort.Strings(r.Implied)
	virtual := true
	for _, h := range s.Hardware.Targets {
		if o, ok := c.byID["hw."+h]; !ok || !o.Virtual {
			virtual = false
		}
	}
	r.Virtual = virtual
	pk, comp, risk := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, id := range r.Selected {
		o := c.byID[id]
		for _, p := range o.PackagesFor(base, virtual) {
			pk[p] = true
		}
		for _, x := range o.Components {
			comp[x] = true
		}
		if o.Risk != nil {
			for _, x := range o.Risk.Flags {
				risk[x] = true
			}
		}
	}
	r.Packages, r.Components, r.Risk = keys(pk), keys(comp), keys(risk)
	r.OK = true
	return r
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
