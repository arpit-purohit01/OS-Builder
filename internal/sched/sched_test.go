package sched

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestDRRFairnessByWeight(t *testing.T) {
	d := NewDRR(10, func(org string) float64 { return map[string]float64{"pro": 4, "free": 1}[org] })
	for i := 0; i < 400; i++ {
		d.Push(Item{ID: fmt.Sprint("p", i), Org: "pro", Cost: 10, Seq: int64(i)})
		d.Push(Item{ID: fmt.Sprint("f", i), Org: "free", Cost: 10, Seq: int64(i)})
	}
	count := map[string]int{}
	for i := 0; i < 500; i++ {
		it, ok := d.Next(nil)
		if !ok {
			t.Fatal("empty")
		}
		count[it.Org]++
	}
	ratio := float64(count["pro"]) / float64(count["free"])
	if math.Abs(ratio-4) > 0.3 {
		t.Fatalf("pro:free = %v (%v)", ratio, count)
	}
}

func TestDRRNoStarvationAndPriority(t *testing.T) {
	d := NewDRR(5, nil)
	d.Push(Item{ID: "big", Org: "a", Cost: 60, Seq: 1})
	for i := 0; i < 100; i++ {
		d.Push(Item{ID: fmt.Sprint("s", i), Org: "b", Cost: 1, Seq: int64(i)})
	}
	got := false
	for i := 0; i < 100; i++ {
		it, _ := d.Next(nil)
		if it.ID == "big" {
			got = true
			break
		}
	}
	if !got {
		t.Fatal("large item starved")
	}
	d2 := NewDRR(100, nil)
	d2.Push(Item{ID: "low", Org: "a", Cost: 1, Priority: 0, Seq: 1})
	d2.Push(Item{ID: "high", Org: "a", Cost: 1, Priority: 5, Seq: 2})
	if it, _ := d2.Next(nil); it.ID != "high" {
		t.Fatalf("priority ignored: %s", it.ID)
	}
	// canRun blocks an org (concurrency cap) — other orgs still progress.
	d3 := NewDRR(10, nil)
	d3.Push(Item{ID: "x", Org: "capped", Cost: 1})
	d3.Push(Item{ID: "y", Org: "open", Cost: 1})
	it, ok := d3.Next(func(i Item) bool { return i.Org != "capped" })
	if !ok || it.ID != "y" {
		t.Fatal(it, ok)
	}
	if _, ok := d3.Next(func(i Item) bool { return i.Org != "capped" }); ok {
		t.Fatal("capped org dispatched")
	}
}

func TestRendezvousStabilityAndBalance(t *testing.T) {
	hosts := []Host{{ID: "h1", Weight: 1, Free: 1}, {ID: "h2", Weight: 1, Free: 1}, {ID: "h3", Weight: 1, Free: 1}, {ID: "h4", Weight: 1, Free: 1}}
	assign := map[string]string{}
	count := map[string]int{}
	for i := 0; i < 4000; i++ {
		k := fmt.Sprint("key", i)
		h, _ := Rendezvous(k, hosts)
		assign[k] = h.ID
		count[h.ID]++
	}
	for _, h := range hosts {
		if count[h.ID] < 800 || count[h.ID] > 1200 {
			t.Fatalf("unbalanced %v", count)
		}
	}
	// Remove h2: only its keys move.
	reduced := []Host{hosts[0], hosts[2], hosts[3]}
	for k, was := range assign {
		h, _ := Rendezvous(k, reduced)
		if was != "h2" && h.ID != was {
			t.Fatalf("key %s moved from %s to %s", k, was, h.ID)
		}
	}
	// Weighted: double weight ≈ double share.
	w := []Host{{ID: "a", Weight: 2, Free: 1}, {ID: "b", Weight: 1, Free: 1}}
	c := map[string]int{}
	for i := 0; i < 9000; i++ {
		h, _ := Rendezvous(fmt.Sprint(i), w)
		c[h.ID]++
	}
	if r := float64(c["a"]) / float64(c["b"]); r < 1.8 || r > 2.2 {
		t.Fatalf("weighted ratio %v", r)
	}
	if _, ok := Rendezvous("k", []Host{{ID: "x", Free: 0}}); ok {
		t.Fatal("full host chosen")
	}
}

func TestGCRA(t *testing.T) {
	now := time.Unix(0, 0)
	g := NewGCRA(1, 3) // 1/s, burst 3
	g.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := g.Allow("org"); !ok {
			t.Fatalf("burst %d denied", i)
		}
	}
	ok, retry := g.Allow("org")
	if ok || retry <= 0 || retry > time.Second {
		t.Fatalf("4th allowed=%v retry=%v", ok, retry)
	}
	now = now.Add(time.Second)
	if ok, _ := g.Allow("org"); !ok {
		t.Fatal("not refilled")
	}
	if ok, _ := g.Allow("other"); !ok {
		t.Fatal("keys not independent")
	}
}

func TestRolloutBucketMonotoneAndIndependent(t *testing.T) {
	in5, in25 := 0, 0
	overlap := 0
	for i := 0; i < 20000; i++ {
		d := fmt.Sprint("dev", i)
		a := Eligible(d, "rel-1", 5)
		b := Eligible(d, "rel-1", 25)
		if a && !b {
			t.Fatal("not monotone")
		}
		if a {
			in5++
		}
		if b {
			in25++
		}
		if a && Eligible(d, "rel-2", 5) {
			overlap++
		}
	}
	if in5 < 850 || in5 > 1150 || in25 < 4700 || in25 > 5300 {
		t.Fatalf("fractions %d %d", in5, in25)
	}
	if overlap > 120 { // independent: expected 5% of 5% ≈ 50
		t.Fatalf("canary sets correlated across releases: %d", overlap)
	}
}

func TestWilsonAndGate(t *testing.T) {
	if lb := WilsonLower(100, 100, 2.326); lb < 0.94 || lb > 0.96 {
		t.Fatalf("lb %v", lb)
	}
	if d, _ := Gate(49, 49, 50, 0.9, 0.98); d != GateWait {
		t.Fatal("advanced below min devices")
	}
	if d, _ := Gate(1000, 1000, 50, 0.99, 0.98); d != GateAdvance {
		t.Fatal(d)
	}
	if d, _ := Gate(80, 100, 50, 0.99, 0.98); d != GatePause {
		t.Fatal(d)
	}
}

func TestDRRRemove(t *testing.T) {
	d := NewDRR(10, nil)
	d.Push(Item{ID: "a", Org: "x", Cost: 1, Seq: 1})
	d.Push(Item{ID: "b", Org: "x", Cost: 1, Seq: 2})
	if !d.Has("a") || !d.Remove("a") || d.Has("a") || d.Remove("a") {
		t.Fatal("remove")
	}
	if it, ok := d.Next(nil); !ok || it.ID != "b" {
		t.Fatalf("next after remove: %+v", it)
	}
}
