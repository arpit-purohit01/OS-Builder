package catalog

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foundry.dev/foundry/internal/osspec"
)

func root(t *testing.T) string {
	d, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		d = filepath.Dir(d)
	}
	t.Fatal("no root")
	return ""
}

func load(t *testing.T) *Catalog {
	c, err := LoadDir(filepath.Join(root(t), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func spec(t *testing.T, y string) *osspec.Spec {
	l, err := osspec.Load([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return l.Spec
}

func bruteForce(f CNF, assumptions []Lit) bool {
	n := f.NumVars
	for m := 0; m < 1<<n; m++ {
		val := func(l Lit) bool {
			v := abs(l)
			b := m>>(v-1)&1 == 1
			if l < 0 {
				return !b
			}
			return b
		}
		ok := true
		for _, a := range assumptions {
			if !val(a) {
				ok = false
				break
			}
		}
		for _, cl := range f.Clauses {
			if !ok {
				break
			}
			sat := false
			for _, l := range cl {
				if val(l) {
					sat = true
					break
				}
			}
			ok = sat
		}
		if ok {
			return true
		}
	}
	return false
}

func TestSolverAgainstBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 3000; iter++ {
		n := 1 + rng.Intn(10)
		var f CNF
		f.NumVars = n
		for c := 0; c < rng.Intn(4*n+1); c++ {
			k := 1 + rng.Intn(3)
			var cl []Lit
			for j := 0; j < k; j++ {
				l := 1 + rng.Intn(n)
				if rng.Intn(2) == 0 {
					l = -l
				}
				cl = append(cl, l)
			}
			f.Clauses = append(f.Clauses, cl)
		}
		var as []Lit
		for j := 0; j < rng.Intn(3); j++ {
			l := 1 + rng.Intn(n)
			if rng.Intn(2) == 0 {
				l = -l
			}
			as = append(as, l)
		}
		want := bruteForce(f, as)
		got := Solve(f, as)
		if got.SAT != want {
			t.Fatalf("iter %d: got %v want %v for %+v assume %v", iter, got.SAT, want, f, as)
		}
		if got.SAT {
			for _, cl := range f.Clauses {
				sat := false
				for _, l := range cl {
					if (l > 0) == got.Model[abs(l)] {
						sat = true
					}
				}
				if !sat {
					t.Fatalf("model violates clause %v", cl)
				}
			}
		} else if len(as) > 0 {
			core := MUS(f, as)
			if bruteForce(f, core) {
				t.Fatal("MUS is satisfiable")
			}
			for i := range core {
				trial := append(append([]Lit(nil), core[:i]...), core[i+1:]...)
				if !bruteForce(f, trial) {
					t.Fatalf("MUS not minimal: %v (drop %d still unsat)", core, i)
				}
			}
		}
	}
}

func TestExactlyOneEncoding(t *testing.T) {
	opts := []Option{}
	for _, id := range []string{"g.a", "g.b", "g.c", "g.d", "g.e"} {
		opts = append(opts, Option{ID: id, Kind: "x", Label: id, OneOf: "g"})
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := c.Compile("ubuntu-24.04", "x86_64")
	for mask := 0; mask < 32; mask++ {
		var as []Lit
		count := 0
		for i, id := range []string{"g.a", "g.b", "g.c", "g.d", "g.e"} {
			v, _ := f.Var(id)
			if mask>>i&1 == 1 {
				as = append(as, v)
				count++
			} else {
				as = append(as, -v)
			}
		}
		if got := Solve(f.CNF, as).SAT; got != (count == 1) {
			t.Fatalf("mask %05b: sat=%v count=%d", mask, got, count)
		}
	}
}

func TestCatalogLoadsAndExamplesResolve(t *testing.T) {
	c := load(t)
	if len(c.Version) != 16 {
		t.Fatalf("version %q", c.Version)
	}
	files, _ := filepath.Glob(filepath.Join(root(t), "docs/examples/*.yaml"))
	for _, fn := range files {
		data, _ := os.ReadFile(fn)
		l, err := osspec.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		r := c.Check(l.Spec)
		if !r.OK {
			t.Fatalf("%s: %+v", fn, r.Errors)
		}
		if len(r.Packages) == 0 {
			t.Fatalf("%s: no packages", fn)
		}
	}
}

func TestKioskImpliesBrowser(t *testing.T) {
	c := load(t)
	s := spec(t, `apiVersion: foundry.dev/v1alpha1
kind: OSSpec
metadata: {name: lobby, version: 1.0.0}
foundation: {base: ubuntu-24.04, profile: kiosk-cage}
ecosystem: {kiosk: {url: "https://k.example.com"}}
hardware: {targets: [generic-vm-x86_64]}
`)
	r := c.Check(s)
	if !r.OK {
		t.Fatal(r.Errors)
	}
	if !contains(r.Implied, "app.kiosk-browser") {
		t.Fatalf("implied %v", r.Implied)
	}
	if !contains(r.Packages, "cage") || !contains(r.Packages, "linux-image-virtual") || contains(r.Packages, "linux-image-generic") {
		t.Fatalf("packages %v", r.Packages)
	}
}

func TestConflictExplanation(t *testing.T) {
	c := load(t)
	s := spec(t, `apiVersion: foundry.dev/v1alpha1
kind: OSSpec
metadata: {name: lobby, version: 1.0.0}
foundation: {base: ubuntu-24.04, profile: kiosk-cage}
ecosystem: {kiosk: {url: "https://k.example.com"}}
apps: {preinstalled: ["flatpak:org.mozilla.firefox"], system: [printing.cups]}
`)
	r := c.Check(s)
	if r.OK {
		t.Fatal("expected conflict: firefox requires a desktop")
	}
	var ptrs []string
	for _, e := range r.Errors {
		if e.Code != "CAT-409" {
			t.Fatalf("code %s", e.Code)
		}
		ptrs = append(ptrs, e.Pointer)
	}
	if !contains(ptrs, "/apps/preinstalled/0") || !contains(ptrs, "/foundation/profile") || contains(ptrs, "/apps/system/0") {
		t.Fatalf("explanation should name only profile + firefox, got %v (%s)", ptrs, r.Errors[0].Message)
	}
	if !strings.Contains(r.Errors[0].Message, "Firefox") {
		t.Fatal(r.Errors[0].Message)
	}
}

func TestUnknownAndUnavailable(t *testing.T) {
	c := load(t)
	s := spec(t, `apiVersion: foundry.dev/v1alpha1
kind: OSSpec
metadata: {name: demo, version: 1.0.0}
foundation: {base: ubuntu-24.04, profile: desktop-gnome}
apps: {system: [no.such.thing]}
kernel: {flavor: realtime}
`)
	r := c.Check(s)
	var cs []string
	for _, e := range r.Errors {
		cs = append(cs, e.Code)
	}
	if !contains(cs, "CAT-404") || !contains(cs, "CAT-410") {
		t.Fatalf("codes %v", cs)
	}
}

func TestCompileDeterministic(t *testing.T) {
	c := load(t)
	a := c.Compile("ubuntu-24.04", "x86_64")
	b := c.Compile("ubuntu-24.04", "x86_64")
	if len(a.CNF.Clauses) != len(b.CNF.Clauses) || strings.Join(a.Vars, ",") != strings.Join(b.Vars, ",") {
		t.Fatal("compile not deterministic")
	}
}
