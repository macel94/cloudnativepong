// Package metrics provides the small, dependency-free application metrics
// contract exposed by Cloud Native Pong. Metrics intentionally have no labels:
// room IDs, player names, client addresses, and request contents must never
// become telemetry cardinality.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry is a concurrency-safe collection of fixed-name counters and gauges.
const maxMetricNames = 256

type Registry struct {
	mu       sync.Mutex
	counters map[string]uint64
	gauges   map[string]int64
}

// NewRegistry creates an empty metrics registry.
func NewRegistry() *Registry {
	return &Registry{
		counters: make(map[string]uint64),
		gauges:   make(map[string]int64),
	}
}

// Inc increments a fixed-name counter. Unknown names are accepted to keep the
// contract extensible; callers must still use bounded, code-defined names.
func (r *Registry) Inc(name string) {
	if !validName(name) {
		return
	}
	r.mu.Lock()
	if !r.ensureNameLocked(name) {
		r.mu.Unlock()
		return
	}
	r.counters[name]++
	r.mu.Unlock()
}

// AddGauge adjusts a fixed-name gauge by delta.
func (r *Registry) AddGauge(name string, delta int64) {
	if !validName(name) {
		return
	}
	r.mu.Lock()
	if !r.ensureNameLocked(name) {
		r.mu.Unlock()
		return
	}
	r.gauges[name] += delta
	r.mu.Unlock()
}

// SetGauge sets a fixed-name gauge.
func (r *Registry) SetGauge(name string, value int64) {
	if !validName(name) {
		return
	}
	r.mu.Lock()
	if !r.ensureNameLocked(name) {
		r.mu.Unlock()
		return
	}
	r.gauges[name] = value
	r.mu.Unlock()
}

// Snapshot returns a deterministic copy for tests and diagnostics.
func (r *Registry) Snapshot() (map[string]uint64, map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counters := make(map[string]uint64, len(r.counters))
	for name, value := range r.counters {
		counters[name] = value
	}
	gauges := make(map[string]int64, len(r.gauges))
	for name, value := range r.gauges {
		gauges[name] = value
	}
	return counters, gauges
}

// Handler renders the registry in the Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.Render()))
	})
}

// Render returns the complete bounded metrics exposition.
func (r *Registry) Render() string {
	counters, gauges := r.Snapshot()
	var names []string
	for name := range counters {
		names = append(names, name)
	}
	for name := range gauges {
		if _, ok := counters[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		if value, ok := counters[name]; ok {
			fmt.Fprintf(&b, "%s_total %d\n", name, value)
		}
		if value, ok := gauges[name]; ok {
			fmt.Fprintf(&b, "%s %d\n", name, value)
		}
	}
	return b.String()
}

func (r *Registry) ensureNameLocked(name string) bool {
	if _, ok := r.counters[name]; ok {
		return true
	}
	if _, ok := r.gauges[name]; ok {
		return true
	}
	return len(r.counters)+len(r.gauges) < maxMetricNames
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// FormatValue is kept as a small contract helper for future metric adapters.
func FormatValue(value int64) string { return strconv.FormatInt(value, 10) }
