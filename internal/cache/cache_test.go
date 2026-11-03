package cache

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTinyLFUCapacityNeverExceeded(t *testing.T) {
	c := NewTinyLFU(10_000, 1000)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		k := fmt.Sprint(rng.Intn(500))
		if !c.Get(k) {
			c.Add(k, int64(1+rng.Intn(800)))
		}
		if c.Used() > 10_000 {
			t.Fatalf("over capacity: %d", c.Used())
		}
	}
	if c.Add("huge", 20_000) {
		t.Fatal("object larger than cache admitted")
	}
}

func TestTinyLFUFavoursFrequentItems(t *testing.T) {
	// Zipf-like workload: TinyLFU should beat plain LRU hit rate.
	lfu := NewTinyLFU(100*100, 2000)
	lru := newLRU(100 * 100)
	rng := rand.New(rand.NewSource(7))
	z := rand.NewZipf(rng, 1.2, 1, 5000)
	var hitLFU, hitLRU int
	for i := 0; i < 200000; i++ {
		k := fmt.Sprint(z.Uint64())
		if lfu.Get(k) {
			hitLFU++
		} else {
			lfu.Add(k, 100)
		}
		if lru.get(k) {
			hitLRU++
		} else {
			lru.add(k, 100)
		}
	}
	if hitLFU <= hitLRU {
		t.Fatalf("TinyLFU hits %d not better than LRU %d", hitLFU, hitLRU)
	}
	t.Logf("hit ratio TinyLFU %.3f vs LRU %.3f", float64(hitLFU)/200000, float64(hitLRU)/200000)
}

func TestTinyLFUPinned(t *testing.T) {
	var evicted []string
	c := NewTinyLFU(300, 100)
	c.OnEvict = func(k string, _ int64) { evicted = append(evicted, k) }
	c.Add("base-tree", 200)
	c.Pin("base-tree")
	for i := 0; i < 50; i++ {
		c.Add(fmt.Sprint("x", i), 100)
	}
	for _, e := range evicted {
		if e == "base-tree" {
			t.Fatal("pinned object evicted")
		}
	}
	c.Unpin("base-tree")
}

// simple LRU for comparison
type lru struct {
	cap, used int64
	order     []string
	size      map[string]int64
}

func newLRU(c int64) *lru { return &lru{cap: c, size: map[string]int64{}} }
func (l *lru) get(k string) bool {
	if _, ok := l.size[k]; !ok {
		return false
	}
	l.touch(k)
	return true
}
func (l *lru) touch(k string) {
	for i, x := range l.order {
		if x == k {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.order = append(l.order, k)
}
func (l *lru) add(k string, s int64) {
	l.size[k] = s
	l.used += s
	l.touch(k)
	for l.used > l.cap {
		v := l.order[0]
		l.order = l.order[1:]
		l.used -= l.size[v]
		delete(l.size, v)
	}
}

func TestSingleflight(t *testing.T) {
	var g Group
	var runs atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err, _ := g.Do(context.Background(), "stage-key", func() (any, error) {
				runs.Add(1)
				time.Sleep(50 * time.Millisecond)
				return 42, nil
			})
			if err != nil || v.(int) != 42 {
				t.Error(v, err)
			}
		}()
	}
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("ran %d times", runs.Load())
	}
}

func TestBloomFalsePositiveRate(t *testing.T) {
	n, p := 100000, 0.001
	b := NewBloom(n, p)
	m, k := b.Params()
	if k < 9 || k > 11 || m < 1_400_000 || m > 1_450_000 {
		t.Fatalf("sizing m=%d k=%d", m, k)
	}
	for i := 0; i < n; i++ {
		b.Add(fmt.Sprint("in-", i))
	}
	for i := 0; i < n; i++ {
		if !b.MayContain(fmt.Sprint("in-", i)) {
			t.Fatal("false negative")
		}
	}
	fp := 0
	for i := 0; i < n; i++ {
		if b.MayContain(fmt.Sprint("out-", i)) {
			fp++
		}
	}
	if rate := float64(fp) / float64(n); rate > 3*p {
		t.Fatalf("fp rate %f", rate)
	}
}

func TestCuckoo(t *testing.T) {
	c := NewCuckoo(10000)
	for i := 0; i < 9000; i++ {
		if !c.Add(fmt.Sprint("k", i)) {
			t.Fatalf("full at %d", i)
		}
	}
	for i := 0; i < 9000; i++ {
		if !c.MayContain(fmt.Sprint("k", i)) {
			t.Fatal("false negative")
		}
	}
	for i := 0; i < 4500; i++ {
		c.Delete(fmt.Sprint("k", i))
	}
	present := 0
	for i := 0; i < 4500; i++ {
		if c.MayContain(fmt.Sprint("k", i)) {
			present++
		}
	}
	if present > 100 {
		t.Fatalf("deleted items still present: %d", present)
	}
	for i := 4500; i < 9000; i++ {
		if !c.MayContain(fmt.Sprint("k", i)) {
			t.Fatal("deletion removed another item")
		}
	}
}
