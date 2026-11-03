package catalog

import (
	"fmt"
	"sort"
)

// Formula is the canonical CNF for one (catalog version, base, arch). It is
// served to the web client as JSON and must be byte-identical across runs.
type Formula struct {
	CatalogVersion string              `json:"catalogVersion"`
	Base           string              `json:"base"`
	Arch           string              `json:"arch"`
	Vars           []string            `json:"vars"` // vars[i-1] is the name of var i
	CNF            CNF                 `json:"cnf"`
	Groups         map[string][]string `json:"groups"`  // oneOf group -> option ids
	Origins        []string            `json:"origins"` // per clause: why it exists (for explanations)
	index          map[string]int
}

// Var returns the variable number for an option or virtual id.
func (f *Formula) Var(id string) (int, bool) {
	v, ok := f.index[id]
	return v, ok
}

// Compile turns catalog constraints into CNF for base/arch.
//
// Encodings: requires a→b as (¬a∨b); conflicts (¬a∨¬b); provides p by
// {o1..ok}: (¬oi∨p) and (¬p∨o1∨…∨ok); exactly-one per oneOf group with
// at-least-one plus a sequential-counter at-most-one (Sinz 2005): O(n)
// clauses and n−1 auxiliary variables instead of O(n²) pairwise clauses.
func (c *Catalog) Compile(base, arch string) *Formula {
	f := &Formula{CatalogVersion: c.Version, Base: base, Arch: arch, Groups: map[string][]string{}, index: map[string]int{}}
	names := make([]string, 0, len(c.Options)+len(c.virtual))
	for _, o := range c.Options {
		names = append(names, o.ID)
	}
	var virt []string
	for v := range c.virtual {
		virt = append(virt, v)
	}
	sort.Strings(virt)
	names = append(names, virt...)
	for _, n := range names {
		f.add(n)
	}
	clause := func(origin string, lits ...Lit) {
		f.CNF.Clauses = append(f.CNF.Clauses, lits)
		f.Origins = append(f.Origins, origin)
	}
	for _, o := range c.Options {
		a := f.index[o.ID]
		if !o.Available(base, arch) {
			clause("unavailable:"+o.ID, -a)
		}
		for _, r := range o.Requires {
			clause("requires:"+o.ID+">"+r, -a, f.index[r])
		}
		for _, x := range o.Conflicts {
			clause("conflicts:"+o.ID+"|"+x, -a, -f.index[x])
		}
		if o.OneOf != "" {
			f.Groups[o.OneOf] = append(f.Groups[o.OneOf], o.ID)
		}
	}
	for _, v := range virt {
		p := f.index[v]
		providers := append([]string(nil), c.virtual[v]...)
		sort.Strings(providers)
		back := []Lit{-p}
		for _, o := range providers {
			clause("provides:"+o+">"+v, -f.index[o], p)
			back = append(back, f.index[o])
		}
		clause("provided-by:"+v, back...)
	}
	groups := make([]string, 0, len(f.Groups))
	for g := range f.Groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		ids := f.Groups[g]
		sort.Strings(ids)
		lits := make([]Lit, len(ids))
		for i, id := range ids {
			lits[i] = f.index[id]
		}
		clause("oneOf-atleast:"+g, lits...)
		n := len(lits)
		if n < 2 {
			continue
		}
		s := make([]Lit, n-1)
		for i := 0; i < n-1; i++ {
			s[i] = f.add(fmt.Sprintf("#amo:%s:%d", g, i))
		}
		origin := "oneOf-atmost:" + g
		clause(origin, -lits[0], s[0])
		for i := 1; i < n-1; i++ {
			clause(origin, -lits[i], s[i])
			clause(origin, -s[i-1], s[i])
			clause(origin, -lits[i], -s[i-1])
		}
		clause(origin, -lits[n-1], -s[n-2])
	}
	f.CNF.NumVars = len(f.Vars)
	return f
}

func (f *Formula) add(name string) int {
	if v, ok := f.index[name]; ok {
		return v
	}
	f.Vars = append(f.Vars, name)
	f.index[name] = len(f.Vars)
	return len(f.Vars)
}
