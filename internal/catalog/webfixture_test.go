package catalog

import (
	"encoding/json"
	"flag"
	"math/rand/v2"
	"os"
	"testing"
)

var webFixture = flag.Bool("webfixture", false, "write web/src/solver/testdata/cases.json")

// TestWebSolverFixture generates 500 random selections with their Go results;
// web/src/solver/sat.test.ts requires the TypeScript solver to agree exactly.
func TestWebSolverFixture(t *testing.T) {
	c, err := LoadDir("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	f := c.Compile("ubuntu-24.04", "x86_64")
	nOpts := len(c.Options)
	r := rand.New(rand.NewPCG(42, 2026))
	type tc struct {
		Assumptions []Lit  `json:"assumptions"`
		SAT         bool   `json:"sat"`
		Model       []bool `json:"model,omitempty"`
		Implied     []Lit  `json:"implied,omitempty"`
		MUS         []Lit  `json:"mus,omitempty"`
	}
	var cases []tc
	unsat := 0
	for i := 0; i < 500; i++ {
		k := 1 + r.IntN(6)
		var as []Lit
		used := map[int]bool{}
		for len(as) < k {
			v := 1 + r.IntN(nOpts)
			if used[v] {
				continue
			}
			used[v] = true
			if r.IntN(4) == 0 {
				as = append(as, -v)
			} else {
				as = append(as, v)
			}
		}
		res := Solve(f.CNF, as)
		c := tc{Assumptions: as, SAT: res.SAT, Model: res.Model, Implied: res.Implied}
		if !res.SAT {
			c.MUS = MUS(f.CNF, as)
			unsat++
			if len(c.MUS) > 3 {
				t.Logf("MUS of %d choices: %v", len(c.MUS), c.MUS)
			}
		}
		cases = append(cases, c)
	}
	if unsat < 50 || unsat > 450 {
		t.Fatalf("fixture should mix SAT and UNSAT cases, got %d UNSAT", unsat)
	}
	small := 0
	for _, c := range cases {
		if !c.SAT && len(c.MUS) <= 3 {
			small++
		}
	}
	if float64(small) < 0.95*float64(unsat) { // Phase 2 acceptance 2
		t.Fatalf("explanations with ≤3 choices: %d of %d", small, unsat)
	}
	if *webFixture {
		b, _ := json.Marshal(map[string]any{"formula": f, "cases": cases})
		if err := os.WriteFile("../../web/src/solver/testdata/cases.json", b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
