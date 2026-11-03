// Package dag executes build stages as a directed acyclic graph with
// Merkle cache keys (docs/context/build-pipeline.md §3, caching.md §2):
//
//	key(stage) = SHA256("foundry-stage-v1" ‖ name ‖ implVersion ‖ JCS(config) ‖ key(dep₁) ‖ …)
//
// A stage whose key is found in the cache is skipped and its recorded
// outputs reused. Ready stages run in parallel within a CPU budget,
// critical-path-first.
package dag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"foundry.dev/foundry/internal/jcs"
)

// Outputs maps an output name (e.g. "disk") to a content digest.
type Outputs map[string]string

// Stage is one node.
type Stage struct {
	Name        string
	Deps        []string
	ImplVersion string // bump when stage code or tool versions change
	Config      any    // only what affects the output
	CPU         int    // vCPUs used (for the parallelism budget)
	EstMinutes  float64
	Run         func(ctx context.Context, in map[string]Outputs) (Outputs, error)
}

// Cache records stage outputs by key.
type Cache interface {
	Get(ctx context.Context, key string) (Outputs, bool, error)
	Put(ctx context.Context, key string, out Outputs) error
}

// Event reports stage progress.
type Event struct {
	Stage    string
	Key      string
	CacheHit bool
	Start    time.Time
	End      time.Time
	Err      error
}

// Graph is a validated DAG.
type Graph struct {
	stages map[string]*Stage
	order  []string // topological
}

// New validates names, dependencies and acyclicity (Kahn's algorithm).
func New(stages []Stage) (*Graph, error) {
	g := &Graph{stages: map[string]*Stage{}}
	for i := range stages {
		s := &stages[i]
		if s.Name == "" || s.Run == nil {
			return nil, fmt.Errorf("dag: stage %q incomplete", s.Name)
		}
		if _, dup := g.stages[s.Name]; dup {
			return nil, fmt.Errorf("dag: duplicate stage %q", s.Name)
		}
		g.stages[s.Name] = s
	}
	indeg := map[string]int{}
	children := map[string][]string{}
	for _, s := range g.stages {
		indeg[s.Name] += 0
		for _, d := range s.Deps {
			if _, ok := g.stages[d]; !ok {
				return nil, fmt.Errorf("dag: %s depends on unknown %s", s.Name, d)
			}
			indeg[s.Name]++
			children[d] = append(children[d], s.Name)
		}
	}
	var ready []string
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		g.order = append(g.order, n)
		cs := children[n]
		sort.Strings(cs)
		for _, c := range cs {
			indeg[c]--
			if indeg[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if len(g.order) != len(g.stages) {
		return nil, errors.New("dag: cycle detected")
	}
	return g, nil
}

// Order returns the topological order.
func (g *Graph) Order() []string { return append([]string(nil), g.order...) }

// Keys computes every stage's Merkle key.
func (g *Graph) Keys() (map[string]string, error) {
	keys := map[string]string{}
	for _, n := range g.order {
		s := g.stages[n]
		cfg, err := jcs.Marshal(s.Config)
		if err != nil {
			return nil, fmt.Errorf("dag: %s config: %w", n, err)
		}
		h := sha256.New()
		h.Write([]byte("foundry-stage-v1\x00" + s.Name + "\x00" + s.ImplVersion + "\x00"))
		h.Write(cfg)
		deps := append([]string(nil), s.Deps...)
		sort.Strings(deps)
		for _, d := range deps {
			h.Write([]byte("\x00" + d + "=" + keys[d]))
		}
		keys[n] = "sha256:" + hex.EncodeToString(h.Sum(nil))
	}
	return keys, nil
}

// CriticalPath returns, per stage, the longest estimated time to a sink
// (reverse topological DP) — used as dispatch priority.
func (g *Graph) CriticalPath() map[string]float64 {
	children := map[string][]string{}
	for _, s := range g.stages {
		for _, d := range s.Deps {
			children[d] = append(children[d], s.Name)
		}
	}
	cp := map[string]float64{}
	for i := len(g.order) - 1; i >= 0; i-- {
		n := g.order[i]
		best := 0.0
		for _, c := range children[n] {
			if cp[c] > best {
				best = cp[c]
			}
		}
		cp[n] = g.stages[n].EstMinutes + best
	}
	return cp
}

// Run executes the graph. cpuBudget bounds Σ CPU of concurrently running
// stages (a stage needing more than the budget runs alone).
func (g *Graph) Run(ctx context.Context, cache Cache, cpuBudget int, onEvent func(Event)) (map[string]Outputs, error) {
	keys, err := g.Keys()
	if err != nil {
		return nil, err
	}
	cp := g.CriticalPath()
	if cpuBudget <= 0 {
		cpuBudget = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mu sync.Mutex
	results := map[string]Outputs{}
	remaining := map[string]int{}
	for n, s := range g.stages {
		remaining[n] = len(s.Deps)
	}
	children := map[string][]string{}
	for _, s := range g.stages {
		for _, d := range s.Deps {
			children[d] = append(children[d], s.Name)
		}
	}
	var ready []string
	for _, n := range g.order {
		if remaining[n] == 0 {
			ready = append(ready, n)
		}
	}
	running, usedCPU := 0, 0
	var firstErr error
	done := make(chan string)
	emit := func(e Event) {
		if onEvent != nil {
			onEvent(e)
		}
	}
	start := func(n string) {
		s := g.stages[n]
		cpu := max(s.CPU, 1)
		running++
		usedCPU += cpu
		go func() {
			ev := Event{Stage: n, Key: keys[n], Start: time.Now()}
			var out Outputs
			var err error
			if cache != nil {
				if o, ok, cerr := cache.Get(ctx, keys[n]); cerr == nil && ok {
					out, ev.CacheHit = o, true
				}
			}
			if !ev.CacheHit {
				in := map[string]Outputs{}
				mu.Lock()
				for _, d := range s.Deps {
					in[d] = results[d]
				}
				mu.Unlock()
				out, err = s.Run(ctx, in)
				if err == nil && cache != nil {
					err = cache.Put(ctx, keys[n], out)
				}
			}
			ev.End, ev.Err = time.Now(), err
			emit(ev)
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("stage %s: %w", n, err)
				cancel()
			}
			if err == nil {
				results[n] = out
			}
			mu.Unlock()
			done <- n
		}()
	}
	for len(ready) > 0 || running > 0 {
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if !failed {
			sort.SliceStable(ready, func(i, j int) bool { return cp[ready[i]] > cp[ready[j]] })
			var still []string
			for _, n := range ready {
				cpu := max(g.stages[n].CPU, 1)
				if usedCPU+cpu <= cpuBudget || running == 0 {
					start(n)
				} else {
					still = append(still, n)
				}
			}
			ready = still
		} else {
			ready = nil
		}
		if running == 0 {
			break
		}
		n := <-done
		running--
		usedCPU -= max(g.stages[n].CPU, 1)
		mu.Lock()
		ok := results[n] != nil || firstErr == nil
		mu.Unlock()
		if ok {
			for _, c := range children[n] {
				remaining[c]--
				if remaining[c] == 0 {
					ready = append(ready, c)
				}
			}
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// ------------------------------------------------------------- memory cache

// MemCache is an in-memory Cache (tests, single-process tools).
type MemCache struct {
	mu sync.Mutex
	m  map[string]Outputs
}

func (c *MemCache) Get(_ context.Context, k string) (Outputs, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.m[k]
	return o, ok, nil
}

func (c *MemCache) Put(_ context.Context, k string, o Outputs) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]Outputs{}
	}
	c.m[k] = o
	return nil
}

// MarshalOutputs is a stable JSON encoding for cache indexes.
func MarshalOutputs(o Outputs) []byte {
	b, _ := json.Marshal(o)
	return b
}
