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
	"time"
)

// Registry is a concurrency-safe collection of fixed-name counters, gauges,
// and bounded aggregate duration distributions.
const maxMetricNames = 256

// HistogramBucketBounds returns the fixed inclusive duration bounds used by
// ObserveDuration. The exported copy lets tests and documentation inspect the
// contract without allowing callers to mutate the registry's buckets.
func HistogramBucketBounds() []time.Duration {
	return append([]time.Duration(nil), latencyBucketBounds...)
}

var latencyBucketBounds = []time.Duration{
	5 * time.Millisecond,
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2500 * time.Millisecond,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
	5 * time.Minute,
}

type durationHistogram struct {
	buckets []uint64
	sum     time.Duration
	count   uint64
}

type Registry struct {
	mu         sync.Mutex
	counters   map[string]uint64
	gauges     map[string]int64
	histograms map[string]*durationHistogram
	// names contains logical metric-family names. A duration distribution
	// reserves one logical name even though it renders several fixed series.
	names map[string]struct{}
}

// NewRegistry creates an empty metrics registry.
func NewRegistry() *Registry {
	return &Registry{
		counters:   make(map[string]uint64),
		gauges:     make(map[string]int64),
		histograms: make(map[string]*durationHistogram),
		names:      make(map[string]struct{}),
	}
}

// RegisterCounter reserves a fixed-name counter without incrementing it. This
// makes canonical zero-valued series visible to private scrapers at startup.
func (r *Registry) RegisterCounter(name string) {
	if !validName(name) {
		return
	}
	r.mu.Lock()
	if _, exists := r.counters[name]; exists {
		r.mu.Unlock()
		return
	}
	if _, exists := r.gauges[name]; exists {
		r.mu.Unlock()
		return
	}
	if _, exists := r.histograms[name]; exists || !r.ensureNameLocked(name) {
		r.mu.Unlock()
		return
	}
	r.counters[name] = 0
	r.mu.Unlock()
}

// RegisterGauge reserves a fixed-name gauge without changing its value.
func (r *Registry) RegisterGauge(name string) {
	if !validName(name) {
		return
	}
	r.mu.Lock()
	if _, exists := r.gauges[name]; exists {
		r.mu.Unlock()
		return
	}
	if _, exists := r.counters[name]; exists {
		r.mu.Unlock()
		return
	}
	if _, exists := r.histograms[name]; exists || !r.ensureNameLocked(name) {
		r.mu.Unlock()
		return
	}
	r.gauges[name] = 0
	r.mu.Unlock()
}

// Inc increments a fixed-name counter. Unknown names are accepted to keep the
// contract extensible; callers must still use bounded, code-defined names.
func (r *Registry) Inc(name string) {
	if !validName(name) {
		return
	}
	r.mu.Lock()
	if _, exists := r.gauges[name]; exists {
		r.mu.Unlock()
		return
	}
	if _, exists := r.histograms[name]; exists || !r.ensureNameLocked(name) {
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
	if _, exists := r.counters[name]; exists {
		r.mu.Unlock()
		return
	}
	if _, exists := r.histograms[name]; exists || !r.ensureNameLocked(name) {
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
	if _, exists := r.counters[name]; exists {
		r.mu.Unlock()
		return
	}
	if _, exists := r.histograms[name]; exists || !r.ensureNameLocked(name) {
		r.mu.Unlock()
		return
	}
	r.gauges[name] = value
	r.mu.Unlock()
}

// RegisterDuration reserves an aggregate duration family with zero values.
// It is safe to call repeatedly and keeps canonical series visible before the
// first observation.
func (r *Registry) RegisterDuration(name string) {
	if !validName(name) {
		return
	}
	r.mu.Lock()
	if _, ok := r.histograms[name]; !ok && r.reserveHistogramLocked(name) {
		// reserveHistogramLocked creates the zero-valued family.
	}
	r.mu.Unlock()
}

// ObserveDuration records a non-negative duration in a fixed cumulative
// bucket distribution. The rendered series are named
// <name>_bucket_le_<bound>_total, <name>_sum, and <name>_count. Bounds are
// encoded in metric names rather than labels, so every observation remains
// aggregate and the family has a fixed cardinality. The suffix is deliberately
// `le_<bound>` rather than a Prometheus `le` label: this is a valid fixed-name
// distribution contract, not a conventional labelled histogram.
//
// Negative durations are ignored. A duration family is reserved atomically;
// if the registry's global metric-name bound cannot fit the complete family,
// the observation is ignored rather than partially registering it.
func (r *Registry) ObserveDuration(name string, duration time.Duration) {
	if !validName(name) || duration < 0 {
		return
	}

	r.mu.Lock()
	histogram, ok := r.histograms[name]
	if !ok {
		if !r.reserveHistogramLocked(name) {
			r.mu.Unlock()
			return
		}
		histogram = r.histograms[name]
	}

	histogram.count++
	histogram.sum += duration
	for i, bound := range latencyBucketBounds {
		if duration <= bound {
			histogram.buckets[i]++
		}
	}
	// The +Inf bucket is represented by the final fixed series and is always
	// incremented, making the cumulative distribution total explicit.
	histogram.buckets[len(latencyBucketBounds)]++
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

// HistogramSnapshot returns a copy of one aggregate duration distribution.
// The bucket values are cumulative and the final value is the +Inf bucket.
func (r *Registry) HistogramSnapshot(name string) (buckets []uint64, sum time.Duration, count uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	histogram, ok := r.histograms[name]
	if !ok {
		return nil, 0, 0, false
	}
	return append([]uint64(nil), histogram.buckets...), histogram.sum, histogram.count, true
}

// Handler renders the registry in the Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.Render()))
	})
}

// Render returns the complete bounded metrics exposition. Duration families
// intentionally use fixed metric names and no label sets; every bucket, sum,
// and count is emitted as an independent aggregate counter. This avoids
// pretending that a standard labelled Prometheus histogram is being emitted.
func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	samples := make(map[string]string)
	for name, value := range r.counters {
		samples[name+"_total"] = strconv.FormatUint(value, 10)
	}
	for name, value := range r.gauges {
		samples[name] = strconv.FormatInt(value, 10)
	}

	type durationSeries struct{ base string }
	durationSamples := make(map[string]durationSeries)
	for base, histogram := range r.histograms {
		for i, value := range histogram.buckets {
			seriesName := histogramBucketName(base, i)
			samples[seriesName] = strconv.FormatUint(value, 10)
			durationSamples[seriesName] = durationSeries{base: base}
		}
		sumName := base + "_sum"
		countName := base + "_count"
		samples[sumName] = strconv.FormatFloat(histogram.sum.Seconds(), 'g', -1, 64)
		samples[countName] = strconv.FormatUint(histogram.count, 10)
		durationSamples[sumName] = durationSeries{base: base}
		durationSamples[countName] = durationSeries{base: base}
	}

	names := make([]string, 0, len(samples))
	for name := range samples {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	commented := make(map[string]struct{})
	for _, name := range names {
		if family, ok := durationSamples[name]; ok {
			if _, done := commented[family.base]; !done {
				fmt.Fprintf(&b, "# HELP %s Aggregate duration distribution in seconds; fixed cumulative buckets with no labels.\n", family.base)
				commented[family.base] = struct{}{}
			}
			fmt.Fprintf(&b, "# TYPE %s counter\n", name)
		}
		fmt.Fprintf(&b, "%s %s\n", name, samples[name])
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
	if _, reserved := r.names[name]; reserved {
		return false
	}
	if len(r.names) >= maxMetricNames {
		return false
	}
	r.names[name] = struct{}{}
	return true
}

func (r *Registry) reserveHistogramLocked(base string) bool {
	if _, ok := r.histograms[base]; ok {
		return true
	}
	if _, exists := r.names[base]; exists {
		return false
	}
	if len(r.names) >= maxMetricNames {
		return false
	}
	r.names[base] = struct{}{}
	r.histograms[base] = &durationHistogram{buckets: make([]uint64, len(latencyBucketBounds)+1)}
	return true
}

func histogramBucketName(base string, index int) string {
	if index == len(latencyBucketBounds) {
		return base + "_bucket_le_inf_total"
	}
	return base + "_bucket_le_" + durationSuffix(latencyBucketBounds[index]) + "_total"
}

func durationSuffix(duration time.Duration) string {
	switch duration {
	case 5 * time.Millisecond:
		return "0_005"
	case 10 * time.Millisecond:
		return "0_01"
	case 25 * time.Millisecond:
		return "0_025"
	case 50 * time.Millisecond:
		return "0_05"
	case 100 * time.Millisecond:
		return "0_1"
	case 250 * time.Millisecond:
		return "0_25"
	case 500 * time.Millisecond:
		return "0_5"
	case time.Second:
		return "1"
	case 2500 * time.Millisecond:
		return "2_5"
	case 5 * time.Second:
		return "5"
	case 10 * time.Second:
		return "10"
	case 30 * time.Second:
		return "30"
	case 60 * time.Second:
		return "60"
	case 5 * time.Minute:
		return "300"
	default:
		return strconv.FormatInt(duration.Milliseconds(), 10)
	}
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
