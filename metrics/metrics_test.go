package metrics

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestRegistryRendersFixedAggregateMetricsWithoutLabels(t *testing.T) {
	registry := NewRegistry()
	registry.Inc("pong_http_requests")
	registry.Inc("pong_http_requests")
	registry.SetGauge("pong_rooms_active", 3)
	registry.AddGauge("pong_websockets_active", 1)
	registry.AddGauge("pong_websockets_active", -1)

	body := registry.Render()
	for _, want := range []string{
		"pong_http_requests_total 2\n",
		"pong_rooms_active 3\n",
		"pong_websockets_active 0\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Render() = %q, missing %q", body, want)
		}
	}
	if strings.Contains(body, "{") || strings.Contains(body, "}") {
		t.Fatalf("Render() contains labels: %q", body)
	}
}

func TestRegistryRejectsInvalidMetricNames(t *testing.T) {
	registry := NewRegistry()
	registry.Inc("bad metric")
	registry.SetGauge("", 1)
	if got := registry.Render(); got != "" {
		t.Fatalf("invalid metric names rendered: %q", got)
	}
}

func TestRegistryBoundsDistinctMetricNames(t *testing.T) {
	registry := NewRegistry()
	for i := 0; i < maxMetricNames+20; i++ {
		registry.Inc("pong_metric_" + strconv.Itoa(i))
	}
	counters, gauges := registry.Snapshot()
	if len(counters)+len(gauges) != maxMetricNames {
		t.Fatalf("distinct metric names = %d, want %d", len(counters)+len(gauges), maxMetricNames)
	}
}

func TestRegistryHandler(t *testing.T) {
	registry := NewRegistry()
	registry.Inc("pong_requests")
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4") {
		t.Fatalf("content type = %q", got)
	}
}
