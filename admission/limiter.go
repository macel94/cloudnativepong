// Package admission provides bounded in-process admission controls for public
// requests and long-lived connections. It deliberately stores only ephemeral
// client keys and counters; callers must not use names, room IDs, or tokens as
// keys.
package admission

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// RateLimiter limits attempts by key in a fixed time window. The key map is
// bounded so an attacker cannot grow process memory by sending unique keys.
type RateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	entries map[string]rateEntry
}

type rateEntry struct {
	windowStart time.Time
	lastSeen    time.Time
	count       int
}

// NewRateLimiter creates a bounded fixed-window limiter.
func NewRateLimiter(limit int, window time.Duration, maxKeys int) *RateLimiter {
	if maxKeys < 1 {
		maxKeys = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimiter{
		limit:   limit,
		window:  window,
		maxKeys: maxKeys,
		entries: make(map[string]rateEntry),
	}
}

// Allow records one attempt and reports whether it is admitted.
func (l *RateLimiter) Allow(key string) bool {
	return l.AllowAt(key, time.Now())
}

// AllowAt is Allow with an injected clock, useful for deterministic tests.
func (l *RateLimiter) AllowAt(key string, now time.Time) bool {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit <= 0 {
		return false
	}

	entry, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= l.maxKeys {
			l.evictOldest()
		}
		entry = rateEntry{windowStart: now, lastSeen: now}
	}
	if now.Before(entry.windowStart) || now.Sub(entry.windowStart) >= l.window {
		entry.windowStart = now
		entry.count = 0
	}
	entry.lastSeen = now
	if entry.count >= l.limit {
		l.entries[key] = entry
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}

func (l *RateLimiter) evictOldest() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range l.entries {
		if oldestKey == "" || entry.lastSeen.Before(oldest) {
			oldestKey = key
			oldest = entry.lastSeen
		}
	}
	if oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}

// ConcurrencyLimiter bounds active operations both per key and globally.
type ConcurrencyLimiter struct {
	mu      sync.Mutex
	perKey  int
	global  int
	maxKeys int
	active  map[string]int
	total   int
}

// NewConcurrencyLimiter creates a bounded per-key/global concurrency limiter.
// A non-positive perKey or global value rejects all acquisitions.
func NewConcurrencyLimiter(perKey, global, maxKeys int) *ConcurrencyLimiter {
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &ConcurrencyLimiter{
		perKey:  perKey,
		global:  global,
		maxKeys: maxKeys,
		active:  make(map[string]int),
	}
}

// Acquire reserves one operation and returns an idempotent release function.
func (l *ConcurrencyLimiter) Acquire(key string) (release func(), ok bool) {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perKey <= 0 || l.global <= 0 || l.total >= l.global {
		return func() {}, false
	}
	current, exists := l.active[key]
	if !exists && len(l.active) >= l.maxKeys {
		return func() {}, false
	}
	if current >= l.perKey {
		return func() {}, false
	}
	l.active[key] = current + 1
	l.total++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if current := l.active[key]; current > 1 {
				l.active[key] = current - 1
			} else {
				delete(l.active, key)
			}
			if l.total > 0 {
				l.total--
			}
		})
	}, true
}

// Controller contains the public admission controls used by the application.
type Controller struct {
	createRate *RateLimiter
	joinRate   *RateLimiter
	http       *ConcurrencyLimiter
	websocket  *ConcurrencyLimiter
}

// Config controls public admission. All limits are in-process and reset on
// restart; they are a safety boundary, not a replacement for edge controls.
type Config struct {
	Window              time.Duration
	CreatePerWindow     int
	JoinPerWindow       int
	HTTPPerClient       int
	WebSocketsPerClient int
	MaxWebSockets       int
	MaxClients          int
}

// DefaultConfig is intentionally conservative while allowing normal browser
// retries and a small local E2E suite.
var DefaultConfig = Config{
	Window:              time.Minute,
	CreatePerWindow:     10,
	JoinPerWindow:       30,
	HTTPPerClient:       8,
	WebSocketsPerClient: 4,
	MaxWebSockets:       128,
	MaxClients:          4096,
}

// ConfigFromEnv reads positive integer/duration overrides. Invalid or unsafe
// values retain the defaults so a bad deployment setting cannot disable the
// admission boundary accidentally.
func ConfigFromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := DefaultConfig
	if value := getenv("PONG_ADMISSION_WINDOW"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			cfg.Window = parsed
		}
	}
	cfg.CreatePerWindow = positiveEnv(getenv, "PONG_CREATE_RATE_LIMIT", cfg.CreatePerWindow)
	cfg.JoinPerWindow = positiveEnv(getenv, "PONG_JOIN_RATE_LIMIT", cfg.JoinPerWindow)
	cfg.HTTPPerClient = positiveEnv(getenv, "PONG_HTTP_CONCURRENCY_PER_CLIENT", cfg.HTTPPerClient)
	cfg.WebSocketsPerClient = positiveEnv(getenv, "PONG_WS_CONCURRENCY_PER_CLIENT", cfg.WebSocketsPerClient)
	cfg.MaxWebSockets = positiveEnv(getenv, "PONG_WS_CONCURRENCY_GLOBAL", cfg.MaxWebSockets)
	cfg.MaxClients = positiveEnv(getenv, "PONG_ADMISSION_MAX_CLIENTS", cfg.MaxClients)
	return cfg
}

func positiveEnv(getenv func(string) string, name string, fallback int) int {
	value, err := strconv.Atoi(getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// NewController constructs all public admission controls.
func NewController(cfg Config) *Controller {
	if cfg.Window <= 0 {
		cfg.Window = DefaultConfig.Window
	}
	if cfg.MaxClients < 1 {
		cfg.MaxClients = DefaultConfig.MaxClients
	}
	return &Controller{
		createRate: NewRateLimiter(cfg.CreatePerWindow, cfg.Window, cfg.MaxClients),
		joinRate:   NewRateLimiter(cfg.JoinPerWindow, cfg.Window, cfg.MaxClients),
		http:       NewConcurrencyLimiter(cfg.HTTPPerClient, cfg.MaxClients, cfg.MaxClients),
		websocket:  NewConcurrencyLimiter(cfg.WebSocketsPerClient, cfg.MaxWebSockets, cfg.MaxClients),
	}
}

// AllowCreate records a room-create attempt.
func (c *Controller) AllowCreate(key string) bool { return c.createRate.Allow(key) }

// AllowJoin records a room-join attempt.
func (c *Controller) AllowJoin(key string) bool { return c.joinRate.Allow(key) }

// AcquireHTTP reserves one short-lived public HTTP operation.
func (c *Controller) AcquireHTTP(key string) (func(), bool) { return c.http.Acquire(key) }

// AcquireWebSocket reserves one long-lived browser WebSocket.
func (c *Controller) AcquireWebSocket(key string) (func(), bool) { return c.websocket.Acquire(key) }
