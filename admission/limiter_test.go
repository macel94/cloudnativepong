package admission

import (
	"testing"
	"time"
)

func TestRateLimiterWindowAndBoundedKeys(t *testing.T) {
	limiter := NewRateLimiter(2, time.Minute, 2)
	now := time.Unix(100, 0)
	if !limiter.AllowAt("a", now) || !limiter.AllowAt("a", now) {
		t.Fatal("first two attempts should be allowed")
	}
	if limiter.AllowAt("a", now) {
		t.Fatal("third attempt in window should be rejected")
	}
	if !limiter.AllowAt("a", now.Add(time.Minute)) {
		t.Fatal("attempt after window should be allowed")
	}
	if !limiter.AllowAt("b", now) {
		t.Fatal("second key should be allowed")
	}
	if !limiter.AllowAt("c", now) {
		t.Fatal("bounded map should evict an old key")
	}
}

func TestConcurrencyLimiterPerKeyGlobalAndIdempotentRelease(t *testing.T) {
	limiter := NewConcurrencyLimiter(2, 3, 2)
	releaseA1, ok := limiter.Acquire("a")
	if !ok {
		t.Fatal("first acquire should be allowed")
	}
	releaseA2, ok := limiter.Acquire("a")
	if !ok {
		t.Fatal("second acquire for key should be allowed")
	}
	if _, ok := limiter.Acquire("a"); ok {
		t.Fatal("per-key limit should reject third acquire")
	}
	releaseB, ok := limiter.Acquire("b")
	if !ok {
		t.Fatal("global capacity should allow third acquire")
	}
	if _, ok := limiter.Acquire("b"); ok {
		t.Fatal("global capacity should reject fourth acquire")
	}
	releaseA1()
	releaseA1()
	if _, ok := limiter.Acquire("a"); !ok {
		t.Fatal("release should free per-key capacity")
	}
	releaseA2()
	releaseB()
}

func TestControllerSeparatesCreateAndJoinRates(t *testing.T) {
	controller := NewController(Config{
		Window:              time.Minute,
		CreatePerWindow:     1,
		JoinPerWindow:       2,
		HTTPPerClient:       1,
		WebSocketsPerClient: 1,
		MaxWebSockets:       1,
		MaxClients:          4,
	})
	if !controller.AllowCreate("client") || controller.AllowCreate("client") {
		t.Fatal("create rate was not enforced")
	}
	if !controller.AllowJoin("client") || !controller.AllowJoin("client") || controller.AllowJoin("client") {
		t.Fatal("join rate was not enforced")
	}
}
