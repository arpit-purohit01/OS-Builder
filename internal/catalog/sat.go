package catalog

// A small complete SAT solver: DPLL with two-watched-literal unit propagation,
// solving under assumptions. Catalog instances are a few thousand variables
// with mostly binary clauses, which this solves in well under a millisecond.
// The same algorithm is implemented in web/src/solver/sat.ts and both are fed
// the identical canonical CNF (property test keeps them in agreement).

// Lit is a DIMACS-style literal: +v or -v, v >= 1.
type Lit = int

// CNF is a formula over variables 1..NumVars.
type CNF struct {
	NumVars int     `json:"numVars"`
	Clauses [][]Lit `json:"clauses"`
}

type solver struct {
	n       int
	clauses [][]Lit
	watches map[Lit][]int // literal -> clause indexes watching it
	assign  []int8        // 0 unassigned, 1 true, -1 false (index by var)
	trail   []Lit
	units   []Lit // unit clauses
	empty   bool
}

func newSolver(f CNF) *solver {
	s := &solver{n: f.NumVars, watches: map[Lit][]int{}, assign: make([]int8, f.NumVars+1)}
	for _, cl := range f.Clauses {
		cl = dedupe(cl)
		if cl == nil { // tautology
			continue
		}
		switch len(cl) {
		case 0:
			s.empty = true
		case 1:
			s.units = append(s.units, cl[0])
		default:
			idx := len(s.clauses)
			s.clauses = append(s.clauses, cl)
			s.watches[cl[0]] = append(s.watches[cl[0]], idx)
			s.watches[cl[1]] = append(s.watches[cl[1]], idx)
		}
	}
	return s
}

func dedupe(cl []Lit) []Lit {
	seen := map[Lit]bool{}
	out := make([]Lit, 0, len(cl))
	for _, l := range cl {
		if seen[-l] {
			return nil
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

func abs(l Lit) int {
	if l < 0 {
		return -l
	}
	return l
}

func (s *solver) value(l Lit) int8 {
	v := s.assign[abs(l)]
	if l < 0 {
		return -v
	}
	return v
}

func (s *solver) set(l Lit) {
	if l > 0 {
		s.assign[l] = 1
	} else {
		s.assign[-l] = -1
	}
	s.trail = append(s.trail, l)
}

// propagate performs BCP from trail position qhead; returns false on conflict.
func (s *solver) propagate(qhead int) bool {
	for qhead < len(s.trail) {
		falseLit := -s.trail[qhead]
		qhead++
		ws := s.watches[falseLit]
		kept := ws[:0]
		conflict := false
		for i := 0; i < len(ws); i++ {
			ci := ws[i]
			if conflict {
				kept = append(kept, ci)
				continue
			}
			cl := s.clauses[ci]
			if cl[0] == falseLit {
				cl[0], cl[1] = cl[1], cl[0]
			}
			if s.value(cl[0]) == 1 {
				kept = append(kept, ci)
				continue
			}
			moved := false
			for k := 2; k < len(cl); k++ {
				if s.value(cl[k]) != -1 {
					cl[1], cl[k] = cl[k], cl[1]
					s.watches[cl[1]] = append(s.watches[cl[1]], ci)
					moved = true
					break
				}
			}
			if moved {
				continue
			}
			kept = append(kept, ci)
			switch s.value(cl[0]) {
			case -1:
				conflict = true
			case 0:
				s.set(cl[0])
			}
		}
		s.watches[falseLit] = kept
		if conflict {
			return false
		}
	}
	return true
}

func (s *solver) undo(to int) {
	for len(s.trail) > to {
		l := s.trail[len(s.trail)-1]
		s.assign[abs(l)] = 0
		s.trail = s.trail[:len(s.trail)-1]
	}
}

// assume sets literals and propagates; false on conflict.
func (s *solver) assume(lits []Lit) bool {
	for _, l := range lits {
		switch s.value(l) {
		case -1:
			return false
		case 1:
			continue
		}
		start := len(s.trail)
		s.set(l)
		if !s.propagate(start) {
			return false
		}
	}
	return true
}

// search is recursive DPLL (depth bounded by var count).
func (s *solver) search() bool {
	v := 0
	for i := 1; i <= s.n; i++ {
		if s.assign[i] == 0 {
			v = i
			break
		}
	}
	if v == 0 {
		return true
	}
	for _, l := range []Lit{-v, v} { // prefer false: minimal models
		mark := len(s.trail)
		s.set(l)
		if s.propagate(mark) && s.search() {
			return true
		}
		s.undo(mark)
	}
	return false
}

// Result of solving.
type Result struct {
	SAT     bool
	Model   []bool // index by var
	Implied []Lit  // literals forced by propagation from the assumptions
}

// Solve solves f under assumptions.
func Solve(f CNF, assumptions []Lit) Result {
	s := newSolver(f)
	if s.empty {
		return Result{}
	}
	if !s.assume(s.units) || !s.assume(assumptions) {
		return Result{}
	}
	implied := append([]Lit(nil), s.trail...)
	if !s.search() {
		return Result{}
	}
	m := make([]bool, s.n+1)
	for i := 1; i <= s.n; i++ {
		m[i] = s.assign[i] == 1
	}
	return Result{SAT: true, Model: m, Implied: implied}
}

// MUS returns a minimal subset of assumptions that is unsatisfiable together
// with f (deletion-based). Assumptions earlier in the slice are kept in
// preference to later ones being dropped (callers order oldest-first).
func MUS(f CNF, assumptions []Lit) []Lit {
	if Solve(f, assumptions).SAT {
		return nil
	}
	core := append([]Lit(nil), assumptions...)
	for i := len(core) - 1; i >= 0; i-- {
		trial := append(append([]Lit(nil), core[:i]...), core[i+1:]...)
		if !Solve(f, trial).SAT {
			core = trial
		}
	}
	return core
}
