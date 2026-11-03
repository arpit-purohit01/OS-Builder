package dag

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mk(name string, deps []string, cfg any, runs *atomic.Int32) Stage {
	return Stage{Name: name, Deps: deps, Config: cfg, ImplVersion: "1", CPU: 1, EstMinutes: 1,
		Run: func(ctx context.Context, in map[string]Outputs) (Outputs, error) {
			runs.Add(1)
			return Outputs{"out": name}, nil
		}}
}

func TestCycleAndUnknown(t *testing.T) {
	var r atomic.Int32
	if _, err := New([]Stage{mk("a", []string{"b"}, nil, &r), mk("b", []string{"a"}, nil, &r)}); err == nil {
		t.Fatal("cycle accepted")
	}
	if _, err := New([]Stage{mk("a", []string{"zz"}, nil, &r)}); err == nil {
		t.Fatal("unknown dep accepted")
	}
}

func TestMerkleKeysPropagate(t *testing.T) {
	var r atomic.Int32
	build := func(wallpaper string) map[string]string {
		g, err := New([]Stage{
			mk("resolve", nil, map[string]any{"pkgs": []string{"a", "b"}}, &r),
			mk("rootfs", []string{"resolve"}, nil, &r),
			mk("customize", []string{"rootfs"}, map[string]any{"wallpaper": wallpaper}, &r),
			mk("assemble", []string{"customize"}, nil, &r),
		})
		if err != nil {
			t.Fatal(err)
		}
		k, _ := g.Keys()
		return k
	}
	a, b := build("aurora"), build("forest")
	if a["resolve"] != b["resolve"] || a["rootfs"] != b["rootfs"] {
		t.Fatal("upstream keys changed with a downstream config change")
	}
	if a["customize"] == b["customize"] || a["assemble"] == b["assemble"] {
		t.Fatal("downstream keys did not change")
	}
}

func TestCacheSkipsStages(t *testing.T) {
	var r atomic.Int32
	stages := func() []Stage {
		return []Stage{mk("a", nil, 1, &r), mk("b", []string{"a"}, 2, &r), mk("c", []string{"a"}, 3, &r), mk("d", []string{"b", "c"}, 4, &r)}
	}
	cache := &MemCache{}
	g, _ := New(stages())
	if _, err := g.Run(context.Background(), cache, 4, nil); err != nil {
		t.Fatal(err)
	}
	if r.Load() != 4 {
		t.Fatalf("first run %d", r.Load())
	}
	var hits atomic.Int32
	g2, _ := New(stages())
	out, err := g2.Run(context.Background(), cache, 4, func(e Event) {
		if e.CacheHit {
			hits.Add(1)
		}
	})
	if err != nil || r.Load() != 4 || hits.Load() != 4 || out["d"]["out"] != "d" {
		t.Fatalf("second run executed stages: runs=%d hits=%d err=%v", r.Load(), hits.Load(), err)
	}
}

func TestParallelismBudgetAndFailure(t *testing.T) {
	var cur, peak atomic.Int32
	var mu sync.Mutex
	slow := func(name string) Stage {
		return Stage{Name: name, CPU: 2, Run: func(ctx context.Context, in map[string]Outputs) (Outputs, error) {
			n := cur.Add(1)
			mu.Lock()
			if n > peak.Load() {
				peak.Store(n)
			}
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			cur.Add(-1)
			return Outputs{"x": name}, nil
		}}
	}
	g, _ := New([]Stage{slow("a"), slow("b"), slow("c"), slow("d")})
	if _, err := g.Run(context.Background(), nil, 4, nil); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d with budget 4 / cpu 2", peak.Load())
	}
	boom := errors.New("boom")
	var ranAfter atomic.Bool
	g2, _ := New([]Stage{
		{Name: "a", Run: func(context.Context, map[string]Outputs) (Outputs, error) { return nil, boom }},
		{Name: "b", Deps: []string{"a"}, Run: func(context.Context, map[string]Outputs) (Outputs, error) {
			ranAfter.Store(true)
			return Outputs{}, nil
		}},
	})
	if _, err := g2.Run(context.Background(), nil, 2, nil); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if ranAfter.Load() {
		t.Fatal("dependent ran after failure")
	}
}

func TestCriticalPath(t *testing.T) {
	var r atomic.Int32
	s := []Stage{mk("a", nil, 0, &r), mk("b", []string{"a"}, 0, &r), mk("c", []string{"a"}, 0, &r)}
	s[1].EstMinutes = 10
	s[2].EstMinutes = 1
	g, _ := New(s)
	cp := g.CriticalPath()
	if cp["a"] != 11 || cp["b"] != 10 || cp["c"] != 1 {
		t.Fatal(cp)
	}
}
