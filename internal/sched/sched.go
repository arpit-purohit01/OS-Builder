// Package sched holds the scheduling and rollout algorithms (docs/context/
// algorithms-and-data-structures.md §4, §5, §12): deficit round robin across
// organizations, weighted rendezvous hashing for cache-affine host choice,
// GCRA rate limiting, deterministic rollout bucketing and Wilson gates.
package sched

import (
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"sync"
	"time"
)

// ------------------------------------------------------------------- DRR

// Item is a schedulable unit (a queued build).
type Item struct {
	ID       string
	Org      string
	Cost     float64 // predicted build minutes
	Priority int     // higher first within an org
	Seq      int64   // enqueue order (lower first)
}

type orgQueue struct {
	items   itemHeap
	deficit float64
}

type itemHeap []Item

func (h itemHeap) Len() int { return len(h) }
func (h itemHeap) Less(i, j int) bool {
	if h[i].Priority != h[j].Priority {
		return h[i].Priority > h[j].Priority
	}
	return h[i].Seq < h[j].Seq
}
func (h itemHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *itemHeap) Push(x any)   { *h = append(*h, x.(Item)) }
func (h *itemHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// DRR is deficit round robin across orgs, with a per-org priority heap.
// Quantum per round = BaseQuantum × weight(org). Starvation-free: every org
// with work accumulates deficit each round until its head item fits.
type DRR struct {
	BaseQuantum float64
	Weight      func(org string) float64
	queues      map[string]*orgQueue
	ring        []string
	pos         int
	arrived     bool // current org already received its quantum this visit
}

// NewDRR creates a scheduler.
func NewDRR(base float64, weight func(string) float64) *DRR {
	return &DRR{BaseQuantum: base, Weight: weight, queues: map[string]*orgQueue{}}
}

// Push adds an item.
func (d *DRR) Push(it Item) {
	q, ok := d.queues[it.Org]
	if !ok {
		q = &orgQueue{}
		d.queues[it.Org] = q
		d.ring = append(d.ring, it.Org)
		sort.Strings(d.ring)
	}
	heap.Push(&q.items, it)
}

// Remove drops a queued item (e.g. cancelled). Reports whether it was found.
func (d *DRR) Remove(id string) bool {
	for _, q := range d.queues {
		for i, it := range q.items {
			if it.ID == id {
				heap.Remove(&q.items, i)
				return true
			}
		}
	}
	return false
}

// Has reports whether an item is queued.
func (d *DRR) Has(id string) bool {
	for _, q := range d.queues {
		for _, it := range q.items {
			if it.ID == id {
				return true
			}
		}
	}
	return false
}

// Len returns the number of queued items.
func (d *DRR) Len() int {
	n := 0
	for _, q := range d.queues {
		n += q.items.Len()
	}
	return n
}

// Next returns the next item to dispatch, honouring canRun (e.g. per-org
// concurrency caps). ok=false when nothing can run.
//
// Classic DRR: on arriving at an org's queue it receives one quantum; it is
// served while its head fits the deficit; then the pointer moves on.
func (d *DRR) Next(canRun func(Item) bool) (Item, bool) {
	if len(d.ring) == 0 {
		return Item{}, false
	}
	maxCost := 0.0
	for _, q := range d.queues {
		for _, it := range q.items {
			maxCost = math.Max(maxCost, it.Cost)
		}
	}
	// Enough visits for any head to accumulate its cost.
	limit := len(d.ring) * (2 + int(maxCost/math.Max(d.BaseQuantum, 1e-9)))
	for visit := 0; visit <= limit; visit++ {
		org := d.ring[d.pos%len(d.ring)]
		q := d.queues[org]
		if q.items.Len() == 0 {
			q.deficit = 0
			d.advance()
			continue
		}
		head := q.items[0]
		if canRun != nil && !canRun(head) {
			d.advance()
			continue
		}
		if !d.arrived {
			w := 1.0
			if d.Weight != nil {
				w = d.Weight(org)
			}
			q.deficit += d.BaseQuantum * w
			d.arrived = true
		}
		if head.Cost <= q.deficit {
			heap.Pop(&q.items)
			q.deficit -= head.Cost
			if q.items.Len() == 0 {
				q.deficit = 0
				d.advance()
			}
			return head, true
		}
		d.advance()
	}
	return Item{}, false
}

func (d *DRR) advance() {
	d.pos++
	d.arrived = false
}

// -------------------------------------------------------------- rendezvous

// Host is a candidate for placement.
type Host struct {
	ID     string
	Weight float64 // relative capacity (slots)
	Free   int
}

func h64(parts ...string) uint64 {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

// Rendezvous picks the host maximising −w/ln(U(host,key)) among hosts with
// free capacity (weighted highest-random-weight hashing). Removing a host only
// remaps the keys it owned.
func Rendezvous(key string, hosts []Host) (Host, bool) {
	best, bestScore, found := Host{}, math.Inf(-1), false
	for _, h := range hosts {
		if h.Free <= 0 {
			continue
		}
		u := (float64(h64(h.ID, key)>>11) + 0.5) / float64(1<<53) // (0,1)
		w := h.Weight
		if w <= 0 {
			w = 1
		}
		score := -w / math.Log(u)
		if score > bestScore || (score == bestScore && h.ID < best.ID) {
			best, bestScore, found = h, score, true
		}
	}
	return best, found
}

// -------------------------------------------------------------------- GCRA

// GCRA is a generic cell rate limiter (token-bucket equivalent) storing one
// "theoretical arrival time" per key.
type GCRA struct {
	mu    sync.Mutex
	tat   map[string]time.Time
	Rate  float64 // events per second
	Burst int
	Now   func() time.Time
}

// NewGCRA creates a limiter.
func NewGCRA(rate float64, burst int) *GCRA {
	return &GCRA{tat: map[string]time.Time{}, Rate: rate, Burst: burst}
}

// Allow reports whether an event for key is allowed; retryAfter otherwise.
func (g *GCRA) Allow(key string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	t := time.Duration(float64(time.Second) / g.Rate)
	tau := time.Duration(g.Burst) * t
	tat, ok := g.tat[key]
	if !ok || tat.Before(now) {
		tat = now
	}
	if tat.Sub(now) > tau-t+time.Nanosecond {
		return false, tat.Sub(now) - tau + t
	}
	g.tat[key] = tat.Add(t)
	if len(g.tat) > 100000 { // bound memory: drop stale keys
		for k, v := range g.tat {
			if v.Before(now) {
				delete(g.tat, k)
			}
		}
	}
	return true, 0
}

// ----------------------------------------------------------------- rollout

// Bucket maps (device, release) to [0,10000) deterministically and
// independently per release.
func Bucket(deviceID, releaseID string) int {
	sum := sha256.Sum256([]byte(deviceID + releaseID))
	return int(binary.BigEndian.Uint64(sum[:8]) % 10000)
}

// Eligible reports whether a device is inside a rollout percentage.
func Eligible(deviceID, releaseID string, percent float64) bool {
	return float64(Bucket(deviceID, releaseID)) < percent*100
}

// WilsonLower is the Wilson score lower bound of a binomial proportion.
func WilsonLower(successes, n int, z float64) float64 {
	if n == 0 {
		return 0
	}
	p := float64(successes) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	centre := p + z*z/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return (centre - margin) / den
}

// GateDecision for one rollout stage.
type GateDecision string

const (
	GateWait    GateDecision = "wait"
	GateAdvance GateDecision = "advance"
	GatePause   GateDecision = "pause"
)

// Gate evaluates a stage: advance when n ≥ minDevices and the 99% lower
// bound ≥ target; pause when the lower bound drops below pauseBelow.
func Gate(successes, attempts, minDevices int, target, pauseBelow float64) (GateDecision, float64) {
	lb := WilsonLower(successes, attempts, 2.326)
	switch {
	case attempts >= 10 && lb < pauseBelow && float64(successes)/float64(attempts) < pauseBelow:
		return GatePause, lb
	case attempts >= minDevices && lb >= target:
		return GateAdvance, lb
	default:
		return GateWait, lb
	}
}
