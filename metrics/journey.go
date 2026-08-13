package metrics

import "sync"

// Journey metric names are deliberately aggregate and label-free. The
// external synthetic runner is the source of truth for availability; this
// helper is for collectors that ingest those observations and need a stable
// status signal without losing raw event accounting.
const (
	JourneyTotalMetric  = "pong_slo_journey"
	JourneyGoodMetric   = "pong_slo_journey_good"
	JourneyFailedMetric = "pong_slo_journey_failed"
	JourneyStatusMetric = "pong_slo_journey_status"
)

// JourneySnapshot is a point-in-time copy of canonical journey accounting.
// Total, good, and failed are raw observation counts. Status is a derived,
// hysteresis-protected gauge: 1 means healthy and 0 means unhealthy.
type JourneySnapshot struct {
	Total         uint64
	Good          uint64
	Failed        uint64
	Status        int64
	FailureStreak int
	SuccessStreak int
}

// JourneyAccounting records every journey observation and independently
// maintains a debounced status. A single transient failure can therefore keep
// status healthy while still increasing Failed and Total for SLO arithmetic.
// Thresholds must be positive; invalid values are replaced with one.
type JourneyAccounting struct {
	mu                sync.Mutex
	registry          *Registry
	failureThreshold  int
	recoveryThreshold int
	snapshot          JourneySnapshot
}

// NewJourneyAccounting creates a collector-side accounting helper. The
// registry is optional; when present it receives the four fixed aggregate
// series listed above. No failure stage, code, URL, room, or request data is
// emitted as a label or metric name.
func NewJourneyAccounting(registry *Registry, failureThreshold, recoveryThreshold int) *JourneyAccounting {
	if failureThreshold <= 0 {
		failureThreshold = 1
	}
	if recoveryThreshold <= 0 {
		recoveryThreshold = 1
	}
	accounting := &JourneyAccounting{
		registry:          registry,
		failureThreshold:  failureThreshold,
		recoveryThreshold: recoveryThreshold,
		snapshot:          JourneySnapshot{Status: 1},
	}
	if registry != nil {
		registry.RegisterCounter(JourneyTotalMetric)
		registry.RegisterCounter(JourneyGoodMetric)
		registry.RegisterCounter(JourneyFailedMetric)
		registry.RegisterGauge(JourneyStatusMetric)
		registry.SetGauge(JourneyStatusMetric, 1)
	}
	return accounting
}

// Observe records one complete canonical external journey. Good is the raw
// numerator decision for that observation and is never suppressed by status
// hysteresis.
func (a *JourneyAccounting) Observe(good bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.snapshot.Total++
	if good {
		a.snapshot.Good++
		a.snapshot.SuccessStreak++
		a.snapshot.FailureStreak = 0
		if a.snapshot.Status == 0 && a.snapshot.SuccessStreak >= a.recoveryThreshold {
			a.snapshot.Status = 1
		}
	} else {
		a.snapshot.Failed++
		a.snapshot.FailureStreak++
		a.snapshot.SuccessStreak = 0
		if a.snapshot.Status == 1 && a.snapshot.FailureStreak >= a.failureThreshold {
			a.snapshot.Status = 0
		}
	}

	if a.registry != nil {
		a.registry.Inc(JourneyTotalMetric)
		if good {
			a.registry.Inc(JourneyGoodMetric)
		} else {
			a.registry.Inc(JourneyFailedMetric)
		}
		a.registry.SetGauge(JourneyStatusMetric, a.snapshot.Status)
	}
}

// Snapshot returns raw counters and the derived status without exposing
// mutable internal state.
func (a *JourneyAccounting) Snapshot() JourneySnapshot {
	if a == nil {
		return JourneySnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshot
}
