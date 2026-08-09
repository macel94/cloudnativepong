package metrics

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestRegistryRendersBoundedDurationDistributionWithoutLabels(t *testing.T) {
	registry := NewRegistry()
	registry.ObserveDuration("pong_http_request_duration_seconds", 5*time.Millisecond)
	registry.ObserveDuration("pong_http_request_duration_seconds", 75*time.Millisecond)
	registry.ObserveDuration("pong_http_request_duration_seconds", 6*time.Second)

	buckets, sum, count, ok := registry.HistogramSnapshot("pong_http_request_duration_seconds")
	if !ok || count != 3 || sum != 6*time.Second+80*time.Millisecond {
		t.Fatalf("duration snapshot = buckets=%v sum=%s count=%d ok=%v", buckets, sum, count, ok)
	}
	if len(buckets) != len(latencyBucketBounds)+1 || buckets[0] != 1 || buckets[4] != 2 || buckets[len(buckets)-1] != 3 {
		t.Fatalf("duration buckets = %v", buckets)
	}

	body := registry.Render()
	for _, want := range []string{
		"pong_http_request_duration_seconds_bucket_le_0_005_total 1\n",
		"pong_http_request_duration_seconds_bucket_le_0_1_total 2\n",
		"pong_http_request_duration_seconds_bucket_le_inf_total 3\n",
		"pong_http_request_duration_seconds_count 3\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Render() = %q, missing %q", body, want)
		}
	}
	if strings.Contains(body, "{") || strings.Contains(body, "}") || strings.Contains(body, `le="`) {
		t.Fatalf("duration metrics contain labels: %q", body)
	}
}

func TestRegistryIgnoresNegativeDurationAndDoesNotPartiallyReserveFamily(t *testing.T) {
	registry := NewRegistry()
	registry.ObserveDuration("pong_negative", -time.Millisecond)
	if _, _, _, ok := registry.HistogramSnapshot("pong_negative"); ok {
		t.Fatal("negative duration reserved a histogram")
	}
	for i := 0; i < maxMetricNames; i++ {
		registry.Inc("pong_metric_" + strconv.Itoa(i))
	}
	registry.ObserveDuration("pong_over_bound", time.Second)
	if _, _, _, ok := registry.HistogramSnapshot("pong_over_bound"); ok {
		t.Fatal("over-bound duration partially reserved a histogram")
	}
}

func TestSLOContractDefinesExternalJourneyAndPrivateDiagnostics(t *testing.T) {
	contents, err := os.ReadFile("../slo-contract.json")
	if err != nil {
		t.Fatalf("read SLO contract: %v", err)
	}
	var contract struct {
		SchemaVersion string `json:"schema_version"`
		Availability  struct {
			Target    float64 `json:"target"`
			Window    string  `json:"window"`
			SLA       bool    `json:"sla"`
			SLOSource string  `json:"source"`
		} `json:"availability"`
		Journey struct {
			ID             string   `json:"id"`
			GoodConditions []string `json:"good_conditions"`
			FailureStages  []string `json:"failure_stages"`
		} `json:"journey"`
		Diagnostics struct {
			MetricNames []string `json:"metric_names"`
			Labels      []string `json:"labels"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(contents, &contract); err != nil {
		t.Fatalf("decode SLO contract: %v", err)
	}
	if contract.SchemaVersion != "belacca.pong-slo-contract.v1" || contract.Availability.Target != 0.99 || contract.Availability.Window != "30d" || contract.Availability.SLA || contract.Availability.SLOSource != "external-durable-synthetic" {
		t.Fatalf("unexpected availability contract: %+v", contract.Availability)
	}
	if contract.Journey.ID != "pong-user-journey" || len(contract.Journey.GoodConditions) < 5 || len(contract.Journey.FailureStages) < 5 {
		t.Fatalf("incomplete journey contract: %+v", contract.Journey)
	}
	if len(contract.Diagnostics.MetricNames) == 0 || len(contract.Diagnostics.Labels) != 0 {
		t.Fatalf("diagnostic privacy contract = %+v", contract.Diagnostics)
	}
}
