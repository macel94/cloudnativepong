package main

import (
	"math"
	"testing"
)

func TestPercentile(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(values, 0.95); got != 10 {
		t.Fatalf("p95 of 1..10 = %v, want 10", got)
	}
	if got := percentile(values, 0.5); got != 5 {
		t.Fatalf("median of 1..10 = %v, want 5", got)
	}
	if got := percentile(values, 0); got != 1 {
		t.Fatalf("min of 1..10 = %v, want 1", got)
	}
	if got := percentile(nil, 0.95); got != 0 {
		t.Fatalf("percentile of empty slice = %v, want 0", got)
	}
}

func TestPercentileUnsorted(t *testing.T) {
	values := []float64{50, 10, 30, 20, 40}
	if got := percentile(values, 0.95); got != 50 {
		t.Fatalf("p95 of unsorted = %v, want 50", got)
	}
}

func TestStatsOf(t *testing.T) {
	stats := statsOf([]float64{10, 20, 30})
	if stats.Count != 3 || stats.Avg != 20 || stats.Max != 30 {
		t.Fatalf("stats = %+v, want count=3 avg=20 max=30", stats)
	}
	if stats.P95 != 30 {
		t.Fatalf("p95 = %v, want 30", stats.P95)
	}
	empty := statsOf(nil)
	if empty.Count != 0 || empty.Avg != 0 || empty.P95 != 0 || empty.Max != 0 {
		t.Fatalf("empty stats = %+v, want all zero", empty)
	}
}

func TestDiagSampleRingBounds(t *testing.T) {
	ring := diagSampleRing{}
	for i := 0; i < diagMaxSamples*2; i++ {
		ring.add(float64(i))
	}
	if ring.count() != diagMaxSamples {
		t.Fatalf("ring count = %d, want bounded at %d", ring.count(), diagMaxSamples)
	}
	ring.reset()
	if ring.count() != 0 {
		t.Fatalf("ring did not reset; count = %d", ring.count())
	}
}

func TestStatsOfAlmostEqualAverage(t *testing.T) {
	stats := statsOf([]float64{20, 20, 20})
	if math.Abs(stats.Avg-20) > 1e-9 {
		t.Fatalf("average = %v, want 20", stats.Avg)
	}
}