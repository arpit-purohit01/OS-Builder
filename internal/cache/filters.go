package cache

import (
	"context"
	"encoding/binary"
	"hash/maphash"
	"math"
	"math/rand/v2"
	"sync"
)

// ------------------------------------------------------------ singleflight

type call struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Group deduplicates concurrent work for the same key (one executes, others wait).
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do runs fn once per key among concurrent callers; shared reports whether
// the result was produced by another caller.
func (g *Group) Do(ctx context.Context, key string, fn func() (any, error)) (v any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*call{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		done := make(chan struct{})
		go func() { c.wg.Wait(); close(done) }()
		select {
		case <-done:
			return c.val, c.err, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	}
	c := &call{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()
	c.val, c.err = fn()
	c.wg.Done()
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.val, c.err, false
}

// ------------------------------------------------------------------ bloom

// Bloom is a standard Bloom filter with double hashing (Kirsch–Mitzenmacher).
type Bloom struct {
	bits []uint64
	m, k uint64
}

// NewBloom sizes a filter for n items at false-positive rate p:
// m = −n·ln p / (ln 2)², k = (m/n)·ln 2.
func NewBloom(n int, p float64) *Bloom {
	m := uint64(math.Ceil(-float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)))
	if m < 64 {
		m = 64
	}
	k := uint64(math.Round(float64(m) / float64(n) * math.Ln2))
	if k < 1 {
		k = 1
	}
	return &Bloom{bits: make([]uint64, (m+63)/64), m: m, k: k}
}

var seedA, seedB, seedFP = maphash.MakeSeed(), maphash.MakeSeed(), maphash.MakeSeed()

// hash2 gives two independent 64-bit hashes (in-memory filters only: seeds are per process).
func hash2(s string) (uint64, uint64) {
	return maphash.String(seedA, s), maphash.String(seedB, s) | 1
}

func (b *Bloom) Add(s string) {
	h1, h2 := hash2(s)
	for i := uint64(0); i < b.k; i++ {
		pos := (h1 + i*h2) % b.m
		b.bits[pos/64] |= 1 << (pos % 64)
	}
}

// MayContain is false only if s was definitely never added.
func (b *Bloom) MayContain(s string) bool {
	h1, h2 := hash2(s)
	for i := uint64(0); i < b.k; i++ {
		pos := (h1 + i*h2) % b.m
		if b.bits[pos/64]&(1<<(pos%64)) == 0 {
			return false
		}
	}
	return true
}

// Params returns m (bits) and k (hash functions).
func (b *Bloom) Params() (uint64, uint64) { return b.m, b.k }

// ----------------------------------------------------------------- cuckoo

// Cuckoo is a cuckoo filter (4-way buckets, 16-bit fingerprints) supporting
// deletion — used for L2 key existence that GC removes from.
type Cuckoo struct {
	buckets [][4]uint16
	mask    uint64
	count   int
}

// NewCuckoo creates a filter for about n items (95% max load).
func NewCuckoo(n int) *Cuckoo {
	nb := 1
	for nb*4*95/100 < n {
		nb <<= 1
	}
	return &Cuckoo{buckets: make([][4]uint16, nb), mask: uint64(nb - 1)}
}

func (c *Cuckoo) fpIdx(s string) (uint16, uint64) {
	h1, h2 := hash2(s)
	fp := uint16(h2 >> 48)
	if fp == 0 {
		fp = 1
	}
	return fp, h1 & c.mask
}

func (c *Cuckoo) alt(i uint64, fp uint16) uint64 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], fp)
	return (i ^ maphash.Bytes(seedFP, b[:])) & c.mask
}

func (c *Cuckoo) insertAt(i uint64, fp uint16) bool {
	for j := range c.buckets[i] {
		if c.buckets[i][j] == 0 {
			c.buckets[i][j] = fp
			return true
		}
	}
	return false
}

// Add inserts s; false means the filter is full.
func (c *Cuckoo) Add(s string) bool {
	fp, i1 := c.fpIdx(s)
	i2 := c.alt(i1, fp)
	if c.insertAt(i1, fp) || c.insertAt(i2, fp) {
		c.count++
		return true
	}
	i := i1
	if rand.IntN(2) == 1 {
		i = i2
	}
	for n := 0; n < 500; n++ {
		j := rand.IntN(4)
		fp, c.buckets[i][j] = c.buckets[i][j], fp
		i = c.alt(i, fp)
		if c.insertAt(i, fp) {
			c.count++
			return true
		}
	}
	return false
}

// MayContain reports possible membership.
func (c *Cuckoo) MayContain(s string) bool {
	fp, i1 := c.fpIdx(s)
	i2 := c.alt(i1, fp)
	for _, i := range []uint64{i1, i2} {
		for _, f := range c.buckets[i] {
			if f == fp {
				return true
			}
		}
	}
	return false
}

// Delete removes one occurrence of s (only call for items that were added).
func (c *Cuckoo) Delete(s string) bool {
	fp, i1 := c.fpIdx(s)
	i2 := c.alt(i1, fp)
	for _, i := range []uint64{i1, i2} {
		for j, f := range c.buckets[i] {
			if f == fp {
				c.buckets[i][j] = 0
				c.count--
				return true
			}
		}
	}
	return false
}

func (c *Cuckoo) Len() int { return c.count }
