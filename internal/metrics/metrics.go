// Package metrics is a minimal Prometheus-compatible registry (counters,
// gauges, histograms with labels) exposed as text format 0.0.4.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type series struct {
	labels  string
	value   float64
	buckets []float64 // cumulative counts for histograms
	sum     float64
	count   uint64
}

type metric struct {
	name, help, kind string
	bounds           []float64
	mu               sync.Mutex
	series           map[string]*series
}

// Registry holds metrics.
type Registry struct {
	mu      sync.Mutex
	metrics []*metric
	byName  map[string]*metric
}

// Default registry.
var Default = &Registry{byName: map[string]*metric{}}

func (r *Registry) get(name, help, kind string, bounds []float64) *metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.byName[name]; ok {
		return m
	}
	m := &metric{name: name, help: help, kind: kind, bounds: bounds, series: map[string]*series{}}
	r.byName[name] = m
	r.metrics = append(r.metrics, m)
	return m
}

func labelString(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		parts = append(parts, fmt.Sprintf(`%s="%s"`, kv[i], v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (m *metric) s(kv []string) *series {
	l := labelString(kv)
	s, ok := m.series[l]
	if !ok {
		s = &series{labels: l, buckets: make([]float64, len(m.bounds))}
		m.series[l] = s
	}
	return s
}

// Counter increments a counter by v.
func (r *Registry) Counter(name, help string, v float64, labels ...string) {
	m := r.get(name, help, "counter", nil)
	m.mu.Lock()
	m.s(labels).value += v
	m.mu.Unlock()
}

// Gauge sets a gauge.
func (r *Registry) Gauge(name, help string, v float64, labels ...string) {
	m := r.get(name, help, "gauge", nil)
	m.mu.Lock()
	m.s(labels).value = v
	m.mu.Unlock()
}

// DefBuckets are latency buckets in seconds.
var DefBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300, 1800}

// Observe records v in a histogram.
func (r *Registry) Observe(name, help string, bounds []float64, v float64, labels ...string) {
	m := r.get(name, help, "histogram", bounds)
	m.mu.Lock()
	s := m.s(labels)
	for i, b := range m.bounds {
		if v <= b {
			s.buckets[i]++
		}
	}
	s.sum += v
	s.count++
	m.mu.Unlock()
}

// Write emits the text exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := append([]*metric(nil), r.metrics...)
	r.mu.Unlock()
	sort.Slice(ms, func(i, j int) bool { return ms[i].name < ms[j].name })
	for _, m := range ms {
		m.mu.Lock()
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)
		keys := make([]string, 0, len(m.series))
		for k := range m.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := m.series[k]
			if m.kind != "histogram" {
				fmt.Fprintf(w, "%s%s %v\n", m.name, s.labels, s.value)
				continue
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(s.labels, "{"), "}")
			sep := ""
			if inner != "" {
				sep = ","
			}
			for i, b := range m.bounds {
				fmt.Fprintf(w, "%s_bucket{%s%sle=\"%v\"} %v\n", m.name, inner, sep, b, s.buckets[i])
			}
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %v\n", m.name, inner, sep, s.count)
			fmt.Fprintf(w, "%s_sum%s %v\n%s_count%s %v\n", m.name, s.labels, s.sum, m.name, s.labels, s.count)
		}
		m.mu.Unlock()
	}
}

// Handler serves /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.Write(w)
	})
}
