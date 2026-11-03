// Package cache implements the build hosts' L1 cache policy: a size-aware
// W-TinyLFU (docs/context/caching.md §4), plus singleflight and the
// probabilistic filters used to skip remote existence checks.
package cache

import (
	"container/list"
	"hash/maphash"
	"sync"
)

// countMin is a 4-row count-min sketch with 4-bit saturating counters and
// periodic halving ("aging") so old popularity decays.
type countMin struct {
	rows    [4][]uint8 // one counter per byte (values capped at 15)
	mask    uint64
	seeds   [4]maphash.Seed
	adds    int
	resetAt int
}

func newCountMin(width int) *countMin {
	w := 1
	for w < width {
		w <<= 1
	}
	c := &countMin{mask: uint64(w - 1), resetAt: 10 * width}
	for i := range c.rows {
		c.rows[i] = make([]uint8, w)
		c.seeds[i] = maphash.MakeSeed()
	}
	return c
}

func (c *countMin) idx(i int, key string) uint64 {
	return maphash.String(c.seeds[i], key) & c.mask
}

func (c *countMin) add(key string) {
	for i := range c.rows {
		j := c.idx(i, key)
		if c.rows[i][j] < 15 {
			c.rows[i][j]++
		}
	}
	c.adds++
	if c.adds >= c.resetAt {
		for i := range c.rows {
			for j := range c.rows[i] {
				c.rows[i][j] >>= 1
			}
		}
		c.adds /= 2
	}
}

func (c *countMin) estimate(key string) int {
	m := 15
	for i := range c.rows {
		if v := int(c.rows[i][c.idx(i, key)]); v < m {
			m = v
		}
	}
	return m
}

type entry struct {
	key  string
	size int64
	seg  int // 0 window, 1 probation, 2 protected
	pins int
}

// TinyLFU is a byte-budgeted W-TinyLFU: 1% window LRU, main segmented LRU
// (20% probation / 80% protected). Admission from the window into main is
// granted only if the candidate's frequency is ≥ the victims' it displaces.
type TinyLFU struct {
	mu        sync.Mutex
	capacity  int64
	window    int64
	protected int64
	used      [3]int64
	lists     [3]*list.List
	items     map[string]*list.Element
	sketch    *countMin
	OnEvict   func(key string, size int64)
}

// NewTinyLFU creates a policy for capacityBytes, expecting about expectItems entries.
func NewTinyLFU(capacityBytes int64, expectItems int) *TinyLFU {
	if expectItems < 64 {
		expectItems = 64
	}
	t := &TinyLFU{
		capacity:  capacityBytes,
		window:    max(capacityBytes/100, 1),
		protected: capacityBytes * 80 / 100,
		items:     map[string]*list.Element{},
		sketch:    newCountMin(expectItems),
	}
	for i := range t.lists {
		t.lists[i] = list.New()
	}
	return t
}

// Get records an access and reports presence.
func (t *TinyLFU) Get(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sketch.add(key)
	el, ok := t.items[key]
	if !ok {
		return false
	}
	e := el.Value.(*entry)
	switch e.seg {
	case 0:
		t.lists[0].MoveToFront(el)
	case 1: // promote to protected
		t.move(el, 2)
		t.rebalanceProtected()
	case 2:
		t.lists[2].MoveToFront(el)
	}
	return true
}

// Pin prevents eviction while an object is in use by a running build.
func (t *TinyLFU) Pin(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.items[key]; ok {
		el.Value.(*entry).pins++
		return true
	}
	return false
}

func (t *TinyLFU) Unpin(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.items[key]; ok && el.Value.(*entry).pins > 0 {
		el.Value.(*entry).pins--
	}
}

// Add inserts a new object (into the window) and evicts as needed. It
// returns false if the object is larger than the whole cache.
func (t *TinyLFU) Add(key string, size int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if size > t.capacity {
		return false
	}
	t.sketch.add(key)
	if el, ok := t.items[key]; ok {
		e := el.Value.(*entry)
		t.used[e.seg] += size - e.size
		e.size = size
		return true
	}
	e := &entry{key: key, size: size}
	t.items[key] = t.lists[0].PushFront(e)
	t.used[0] += size
	// Window overflow: candidates move to probation via the admission filter.
	for t.used[0] > t.window && t.lists[0].Len() > 1 {
		cand := t.lists[0].Back()
		if cand.Value.(*entry).pins > 0 {
			break
		}
		t.admit(cand)
	}
	t.enforce()
	return true
}

// admit moves a window candidate into main if it beats the victims.
func (t *TinyLFU) admit(cand *list.Element) {
	ce := cand.Value.(*entry)
	need := t.used[1] + t.used[2] + ce.size - (t.capacity - t.window)
	if need <= 0 {
		t.move(cand, 1)
		return
	}
	// Collect probation victims from the LRU end until enough bytes are freed.
	candFreq := t.sketch.estimate(ce.key)
	var victims []*list.Element
	var freed int64
	for el := t.lists[1].Back(); el != nil && freed < need; el = el.Prev() {
		e := el.Value.(*entry)
		if e.pins > 0 {
			continue
		}
		if t.sketch.estimate(e.key) > candFreq {
			// A more popular victim: reject the candidate instead.
			t.evict(cand)
			return
		}
		victims = append(victims, el)
		freed += e.size
	}
	if freed < need {
		// Not enough probation space; fall back to evicting the candidate.
		t.evict(cand)
		return
	}
	for _, v := range victims {
		t.evict(v)
	}
	t.move(cand, 1)
}

func (t *TinyLFU) rebalanceProtected() {
	for t.used[2] > t.protected {
		el := t.lists[2].Back()
		if el == nil {
			return
		}
		t.move(el, 1)
	}
}

// enforce guarantees total usage ≤ capacity (evicting probation LRU first).
func (t *TinyLFU) enforce() {
	for t.used[0]+t.used[1]+t.used[2] > t.capacity {
		var victim *list.Element
		for _, seg := range []int{1, 0, 2} {
			for el := t.lists[seg].Back(); el != nil; el = el.Prev() {
				if el.Value.(*entry).pins == 0 {
					victim = el
					break
				}
			}
			if victim != nil {
				break
			}
		}
		if victim == nil {
			return // everything pinned
		}
		t.evict(victim)
	}
}

func (t *TinyLFU) move(el *list.Element, seg int) {
	e := el.Value.(*entry)
	t.lists[e.seg].Remove(el)
	t.used[e.seg] -= e.size
	e.seg = seg
	t.items[e.key] = t.lists[seg].PushFront(e)
	t.used[seg] += e.size
}

func (t *TinyLFU) evict(el *list.Element) {
	e := el.Value.(*entry)
	t.lists[e.seg].Remove(el)
	t.used[e.seg] -= e.size
	delete(t.items, e.key)
	if t.OnEvict != nil {
		t.OnEvict(e.key, e.size)
	}
}

// Used returns total bytes cached.
func (t *TinyLFU) Used() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.used[0] + t.used[1] + t.used[2]
}

// Len returns the number of cached objects.
func (t *TinyLFU) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items)
}
