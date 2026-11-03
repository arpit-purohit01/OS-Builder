package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := &Registry{byName: map[string]*metric{}}
	r.Counter("foundry_builds_total", "Builds", 1, "state", "queued")
	r.Counter("foundry_builds_total", "Builds", 2, "state", "queued")
	r.Gauge("foundry_queue_depth", "Depth", 7)
	r.Observe("foundry_http_seconds", "Latency", []float64{0.1, 1}, 0.5, "route", "/v1/x")
	var b strings.Builder
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		`foundry_builds_total{state="queued"} 3`,
		`foundry_queue_depth 7`,
		`foundry_http_seconds_bucket{route="/v1/x",le="0.1"} 0`,
		`foundry_http_seconds_bucket{route="/v1/x",le="1"} 1`,
		`foundry_http_seconds_count{route="/v1/x"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}
