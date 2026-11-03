package api

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
)

// TestMain proves acceptance test 1 of Phase 1: every endpoint of the API is
// exercised by the integration tests (only checked when the whole package runs).
func TestMain(m *testing.M) {
	var mu sync.Mutex
	hit := map[string]bool{}
	routeMu.Lock()
	routeHook = func(p string) { mu.Lock(); hit[p] = true; mu.Unlock() }
	routeMu.Unlock()
	code := m.Run()
	if f := flag.Lookup("test.run"); code == 0 && (f == nil || f.Value.String() == "") && !testing.Short() {
		var missing []string
		routeMu.Lock()
		for p := range registered {
			if !hit[p] {
				missing = append(missing, p)
			}
		}
		routeMu.Unlock()
		sort.Strings(missing)
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "endpoints never exercised by a test (%d):\n", len(missing))
			for _, p := range missing {
				fmt.Fprintln(os.Stderr, "  "+p)
			}
			code = 1
		}
	}
	os.Exit(code)
}
