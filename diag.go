// Diagnostics for the realtime path.
//
// The browser-facing snapshot pipeline has hops: room pod stream -> API proxy
// -> Caddy -> browser. Latency or jitter at any hop shows up as frontend lag,
// so each hop records its own frame-gap and write/relay timing into the shared
// bounded metrics registry and emits compact rate-limited summaries to stdout.
// PONG_DIAG=1 additionally enables per-frame lines for fine-grained timelining.
package main

import (
	"math"
	"os"
	"sort"
	"time"
)

// diagSummaryInterval is how often each hop prints its latency summary.
const diagSummaryInterval = 2 * time.Second

// diagMaxSamples bounds the in-memory ring per relay/stream so summaries never
// grow unbounded even on very long games.
const diagMaxSamples = 512

// diagPerFrame reports whether PONG_DIAG=1 enables per-frame verbose logging.
// The rate-limited summaries and duration histograms run regardless so normal
// production logs stay quiet while still capturing the necessary data.
func diagPerFrame() bool {
	return os.Getenv("PONG_DIAG") == "1"
}

// diagSampleRing accumulates float64 timing samples (in milliseconds) for a
// bounded window. The stats are computed from the current contents and then
// the ring is reset so each printed summary covers a fresh interval.
type diagSampleRing struct {
	samples []float64
}

func (r *diagSampleRing) add(v float64) {
	if len(r.samples) < diagMaxSamples {
		r.samples = append(r.samples, v)
	}
}

func (r *diagSampleRing) count() int {
	return len(r.samples)
}

func (r *diagSampleRing) reset() {
	r.samples = r.samples[:0]
}

// diagStats is a compact latency summary for one interval.
type diagStats struct {
	Count int
	Avg   float64
	P95   float64
	Max   float64
}

// percentile returns the p-th percentile (0..1) of the values using nearest
// rank so p95 of 1..10 is 10 (the expected highest latency signal).
func percentile(values []float64, p float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, values)
	sort.Float64s(sorted)
	index := int(math.Ceil(p*float64(n))) - 1
	if index < 0 {
		index = 0
	}
	if index > n-1 {
		index = n-1
	}
	return sorted[index]
}

// statsOf computes Count, Avg, P95, and Max for the interval samples.
func statsOf(values []float64) diagStats {
	if len(values) == 0 {
		return diagStats{}
	}
	sum := 0.0
	max := 0.0
	for _, v := range values {
		sum += v
		if v > max {
			max = v
		}
	}
	return diagStats{
		Count: len(values),
		Avg:   sum / float64(len(values)),
		P95:   percentile(values, 0.95),
		Max:   max,
	}
}