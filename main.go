package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudnativepong/admission"

	"github.com/cloudnativepong/db"
	"github.com/cloudnativepong/game"
	"github.com/cloudnativepong/lobby"
	"github.com/cloudnativepong/metrics"

	"github.com/gorilla/websocket"
)

type originAllowlist map[string]struct{}

var websocketOrigins = defaultOriginAllowlist("local")
var publicAdmission = admission.NewController(admission.DefaultConfig)
var appMetrics = metrics.NewRegistry()

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return websocketOrigins.allowed(r.Header.Get("Origin")) },
}

func defaultOriginAllowlist(mode string) originAllowlist {
	if mode == "lobby" || mode == "kubernetes" || mode == "room" {
		return originAllowlist{"https://pong.belacca.com": {}}
	}
	return originAllowlist{
		"http://localhost:8080": {},
		"http://127.0.0.1:8080": {},
		"http://[::1]:8080":     {},
	}
}

func loadOriginAllowlist(mode, configured string) originAllowlist {
	if strings.TrimSpace(configured) == "" {
		configured = os.Getenv("PONG_ALLOWED_ORIGINS")
	}
	if strings.TrimSpace(configured) == "" {
		return defaultOriginAllowlist(mode)
	}
	origins := make(originAllowlist)
	for _, raw := range strings.Split(configured, ",") {
		if origin, ok := normalizeOrigin(raw); ok {
			origins[origin] = struct{}{}
		}
	}
	if len(origins) == 0 {
		return defaultOriginAllowlist(mode)
	}
	return origins
}

func normalizeOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), true
}

func (a originAllowlist) allowed(origin string) bool {
	canonical, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	_, ok = a[canonical]
	return ok
}

func (a originAllowlist) first() string {
	origins := make([]string, 0, len(a))
	for origin := range a {
		origins = append(origins, origin)
	}
	sort.Strings(origins)
	if len(origins) == 0 {
		return ""
	}
	return origins[0]
}

func configureOriginPolicy(mode, configured string) {
	websocketOrigins = loadOriginAllowlist(mode, configured)
}

func main() {
	mode := flag.String("mode", "local", "Mode: local, lobby, room")
	port := flag.String("port", "8080", "HTTP listen port")
	roomID := flag.String("room-id", "", "Room ID (for room mode)")
	lobbyAddr := flag.String("lobby-addr", "", "Lobby address (for room mode)")
	k8sNS := flag.String("namespace", "default", "Kubernetes namespace")
	roomImage := flag.String("room-image", "cloudnativepong-room:latest", "Room container image")
	dbPath := flag.String("db-path", ":memory:", "SQLite database path")
	allowedOrigins := flag.String("allowed-origins", "", "Comma-separated exact browser origins (or PONG_ALLOWED_ORIGINS)")
	flag.Parse()

	configureOriginPolicy(*mode, *allowedOrigins)
	publicAdmission = admission.NewController(admission.ConfigFromEnv(os.Getenv))

	store, err := db.NewWithMetrics(*dbPath, appMetrics)
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer store.Close()

	lobbySrv := lobby.NewServerWithMetrics(store, *mode, *k8sNS, *roomImage, appMetrics)

	mux := http.NewServeMux()

	switch *mode {
	case "local":
		// In local mode, lobby and rooms share one process.
		// Rooms are spawned as goroutines and addressed via path /room/{id}/ws
		setupLocalRoutes(mux, lobbySrv, store)
		go reconcileLobbyRooms(lobbySrv)

	case "lobby":
		// Lobby-only mode: serves API, delegates rooms to K8s pods.
		// Static files are served by nginx.
		apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/rooms":
				lobbySrv.HandleListRooms(w, r)
			case r.URL.Path == "/api/rooms/create":
				if !publicAdmission.AllowCreate(clientKey(r)) {
					appMetrics.Inc("pong_admission_create_rejected")
					tooManyRequests(w)
					return
				}
				lobbySrv.HandleCreateRoom(w, r)
			case r.URL.Path == "/api/rooms/join":
				if !publicAdmission.AllowJoin(clientKey(r)) {
					appMetrics.Inc("pong_admission_join_rejected")
					tooManyRequests(w)
					return
				}
				lobbySrv.HandleJoinRoom(w, r)
			default:
				http.NotFound(w, r)
			}
		})
		mux.Handle("/api/", publicAPIHandler(corsMiddleware(apiHandler)))
		mux.HandleFunc("/health", healthHandler)
		mux.Handle("/metrics", appMetrics.Handler())
		mux.HandleFunc("/internal/rooms/", func(w http.ResponseWriter, r *http.Request) {
			parts := splitPath(r.URL.Path)
			if len(parts) != 4 || parts[0] != "internal" || parts[1] != "rooms" {
				http.NotFound(w, r)
				return
			}
			switch parts[3] {
			case "started":
				lobbySrv.HandleRoomStarted(w, r, parts[2])
			case "finished":
				lobbySrv.HandleRoomFinished(w, r, parts[2])
			default:
				http.NotFound(w, r)
			}
		})
		go reconcileLobbyRooms(lobbySrv)
		// WebSocket proxy: /rooms/{id}/ws -> room pod
		mux.HandleFunc("/rooms/", func(w http.ResponseWriter, r *http.Request) {
			proxyRoomWS(w, r, lobbySrv, store)
		})

	case "room":
		// Room-only mode: runs a single game room.
		if *roomID == "" {
			log.Fatal("--room-id is required in room mode")
		}
		setupRoomRoutes(mux, *roomID, *lobbyAddr)

	default:
		log.Fatalf("unknown mode: %s", *mode)
	}

	addr := ":" + *port
	log.Printf("Cloud Native Pong starting on %s (mode=%s)", addr, *mode)

	srv := &http.Server{
		Addr:              addr,
		Handler:           requestMetrics(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// Graceful shutdown with timeout. A room container exits after its game
	// finishes so the lobby can remove the completed room resources.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	if *mode == "room" {
		room := getOrCreateLocalRoom(*roomID)
		select {
		case <-room.done:
			// Let clients receive the final state before the room container exits.
			time.Sleep(3 * time.Second)
			notifyRoomFinished(*roomID, *lobbyAddr, newRequestID())
		case <-quit:
		}
	} else {
		<-quit
	}
	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// ---- Local mode: lobby + rooms in one process ----

type localRoom struct {
	engine      *game.Engine
	players     [2]*websocket.Conn
	mu          sync.Mutex
	loopOnce    sync.Once
	done        chan struct{}
	doneOnce    sync.Once
	cleanupOnce sync.Once
	cleanup     func()
	start       func() error
}

var localRooms sync.Map // roomID -> *localRoom

func setupLocalRoutes(mux *http.ServeMux, lobbySrv *lobby.Server, store *db.Store) {
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/metrics", appMetrics.Handler())

	// API routes use the same admission and CORS contract as lobby mode.
	mux.Handle("/api/rooms", publicAPIHandler(corsMiddleware(http.HandlerFunc(lobbySrv.HandleListRooms))))
	mux.Handle("/api/rooms/create", publicAPIHandler(corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !publicAdmission.AllowCreate(clientKey(r)) {
			appMetrics.Inc("pong_admission_create_rejected")
			tooManyRequests(w)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := lobby.DecodeJSONBody(w, r, &req); err != nil {
			lobbyWriteError(w, err)
			return
		}
		name, err := lobby.ValidRoomName(req.Name)
		if err != nil {
			lobbyWriteError(w, err)
			return
		}
		room, err := lobbySrv.CreateRoom(name)
		if err != nil {
			appMetrics.Inc("pong_room_create_http_failure")
			http.Error(w, "room service unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := lobbySrv.JoinRoom(room.ID); err != nil {
			_ = lobbySrv.CleanupRoom(room.ID)
			appMetrics.Inc("pong_room_create_http_failure")
			lobbyWriteError(w, err)
			return
		}
		room.Players = 1
		lobbySrv.RegisterLocalRoom(room.ID, &lobby.RoomHandler{
			ID:   room.ID,
			Addr: fmt.Sprintf("localhost:%s/room/%s/ws", "8080", room.ID),
		})
		setLocalRoomCallbacks(room.ID,
			func() error {
				if err := lobbySrv.MarkRoomStarted(room.ID); err != nil {
					appMetrics.Inc("pong_room_start_callback_failure")
					return err
				}
				appMetrics.Inc("pong_room_start_callback_success")
				return nil
			},
			func() {
				_ = lobbySrv.CleanupRoom(room.ID)
				localRooms.Delete(room.ID)
			},
		)
		appMetrics.Inc("pong_room_create_http_success")
		writeJSON(w, room)
	}))))
	mux.Handle("/api/rooms/join", publicAPIHandler(corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !publicAdmission.AllowJoin(clientKey(r)) {
			appMetrics.Inc("pong_admission_join_rejected")
			tooManyRequests(w)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			RoomID string `json:"room_id"`
		}
		if err := lobby.DecodeJSONBody(w, r, &req); err != nil {
			lobbyWriteError(w, err)
			return
		}
		if !lobby.ValidRoomID(req.RoomID) {
			lobbyWriteError(w, lobby.ErrInvalidRoomID)
			return
		}
		if err := lobbySrv.JoinRoom(req.RoomID); err != nil {
			appMetrics.Inc("pong_room_join_http_failure")
			lobbyWriteError(w, err)
			return
		}
		appMetrics.Inc("pong_room_join_http_success")
		writeJSON(w, map[string]string{"room_id": req.RoomID, "mode": "local"})
	}))))

	// Room WebSocket endpoint (both legacy and gateway paths)
	mux.HandleFunc("/room/", func(w http.ResponseWriter, r *http.Request) {
		// Parse /room/{id}/ws
		path := r.URL.Path
		parts := splitPath(path)
		if len(parts) < 3 || parts[2] != "ws" {
			http.NotFound(w, r)
			return
		}
		handleRoomWS(w, r, parts[1], "")
	})

	// Gateway WebSocket route: /rooms/{id}/ws
	mux.HandleFunc("/rooms/", func(w http.ResponseWriter, r *http.Request) {
		// Parse /rooms/{id}/ws
		path := r.URL.Path
		parts := splitPath(path)
		if len(parts) < 3 || parts[2] != "ws" {
			http.NotFound(w, r)
			return
		}
		handleRoomWS(w, r, parts[1], "")
	})

	// Static files (local mode only — served by nginx in K8s)
	mux.Handle("/", http.FileServer(http.Dir("./static")))
}

func setupRoomRoutes(mux *http.ServeMux, roomID, lobbyAddr string) {
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleRoomWS(w, r, roomID, lobbyAddr)
	})
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/metrics", appMetrics.Handler())
}

func handleRoomWS(w http.ResponseWriter, r *http.Request, roomID, lobbyAddr string) {
	if !lobby.ValidRoomID(roomID) {
		appMetrics.Inc("pong_websocket_rejected_invalid")
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}
	release, ok := publicAdmission.AcquireWebSocket(clientKey(r))
	if !ok {
		appMetrics.Inc("pong_admission_websocket_rejected")
		tooManyRequests(w)
		return
	}
	defer release()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		appMetrics.Inc("pong_websocket_upgrade_failure")
		return
	}
	appMetrics.Inc("pong_websocket_upgrade_success")
	appMetrics.Inc("pong_websocket_accepted")
	appMetrics.AddGauge("pong_websockets_active", 1)
	defer appMetrics.AddGauge("pong_websockets_active", -1)
	defer conn.Close()
	enableTCPNoDelay(conn.UnderlyingConn())

	// Get or create the local room
	room := getOrCreateLocalRoom(roomID)

	// Assign player slot
	room.mu.Lock()
	var player int
	if room.players[0] == nil {
		player = 1
		room.players[0] = conn
	} else if room.players[1] == nil {
		player = 2
		room.players[1] = conn
	} else {
		room.mu.Unlock()
		appMetrics.Inc("pong_websocket_room_full")
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": "room full"})
		return
	}
	room.mu.Unlock()
	appMetrics.Inc("pong_websocket_player_assigned")

	// Notify player of their assignment.
	if err := conn.WriteJSON(map[string]interface{}{
		"type":   "joined",
		"player": player,
	}); err != nil {
		appMetrics.Inc("pong_websocket_joined_write_failure")
		room.signalFinished()
		return
	}
	appMetrics.Inc("pong_websocket_joined_write_success")

	// Mark ready. The lobby's playing transition is deliberately based on
	// actual WebSocket connections, not only on API reservations.
	room.engine.PlayerReady(player)
	if player == 2 {
		room.mu.Lock()
		start := room.start
		room.mu.Unlock()
		var startErr error
		if lobbyAddr == "" {
			if start != nil {
				startErr = start()
			}
		} else {
			startErr = notifyRoomStarted(roomID, lobbyAddr, requestID(r))
		}
		if startErr != nil {
			// Do not leave a live game in waiting state: reconciliation would
			// eventually expire it as an abandoned room.
			room.signalFinished()
			return
		}
	}

	// Read loop: handle input from this player
	go func() {
		for {
			var input game.Input
			err := conn.ReadJSON(&input)
			if err != nil {
				appMetrics.Inc("pong_websocket_disconnect")
				room.engine.PlayerLeft(player)
				room.signalFinished()
				// Close the other player's connection
				room.mu.Lock()
				other := 0
				if player == 1 {
					other = 1
				}
				if room.players[other] != nil {
					room.players[other].Close()
				}
				room.mu.Unlock()
				return
			}
			input.Player = player
			room.engine.ApplyInput(input)
		}
	}()

	// Start the loop once for every room so a single-player disconnect can
	// also terminate an abandoned room. PlayerReady above starts the engine
	// when the second actual WebSocket has connected.
	room.loopOnce.Do(func() { go runGameLoop(room) })

	// Keep connection alive (read loop above will exit on disconnect)
	// This goroutine writes game state to the client
	ticker := time.NewTicker(game.TickDuration)
	defer ticker.Stop()

	for range ticker.C {
		state := room.engine.State()
		err := conn.WriteJSON(map[string]interface{}{
			"type":  "state",
			"state": state,
		})
		if err != nil {
			appMetrics.Inc("pong_websocket_state_write_failure")
			room.signalFinished()
			return
		}
		appMetrics.Inc("pong_websocket_state_write_success")
		if state.Status == game.StatusFinished {
			// Send final state and close. The room-mode process also watches the
			// room completion signal and reports it to the lobby.
			appMetrics.Inc("pong_room_finished")
			room.signalFinished()
			time.Sleep(3 * time.Second)
			localRooms.Delete(roomID)
			return
		}
	}
}

func runGameLoop(room *localRoom) {
	ticker := time.NewTicker(game.TickDuration)
	defer ticker.Stop()

	for range ticker.C {
		state := room.engine.Tick()
		if state.Status == game.StatusFinished {
			room.signalFinished()
			// Give clients time to see the final state
			time.Sleep(3 * time.Second)
			room.mu.Lock()
			for _, c := range room.players {
				if c != nil {
					c.Close()
				}
			}
			room.mu.Unlock()
			return
		}
	}
}

func getOrCreateLocalRoom(id string) *localRoom {
	v, _ := localRooms.LoadOrStore(id, &localRoom{
		engine: game.NewEngine(),
		done:   make(chan struct{}),
	})
	return v.(*localRoom)
}

func setLocalRoomCallbacks(id string, start func() error, cleanup func()) {
	room := getOrCreateLocalRoom(id)
	room.mu.Lock()
	room.start = start
	room.cleanup = cleanup
	room.mu.Unlock()
}

func (r *localRoom) signalFinished() {
	r.doneOnce.Do(func() {
		close(r.done)
		r.mu.Lock()
		cleanup := r.cleanup
		r.mu.Unlock()
		r.cleanupOnce.Do(func() {
			if cleanup != nil {
				cleanup()
				appMetrics.Inc("pong_room_cleanup_callback_success")
			}
		})
	})
}

const roomReconcileInterval = time.Minute

func reconcileLobbyRooms(lobbySrv *lobby.Server) {
	if err := lobbySrv.ReconcileRooms(); err != nil {
		log.Printf("event=room_reconciliation_failed")
	}
	ticker := time.NewTicker(roomReconcileInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := lobbySrv.ReconcileRooms(); err != nil {
			log.Printf("event=room_reconciliation_failed")
		}
	}
}

func notifyRoomStarted(roomID, lobbyAddr, correlationID string) error {
	if lobbyAddr == "" {
		return nil
	}
	url := "http://" + lobbyAddr + "/internal/rooms/" + roomID + "/started"
	client := &http.Client{Timeout: 5 * time.Second}
	for attempt := 0; attempt < 3; attempt++ {
		appMetrics.Inc("pong_room_start_callback_attempt")
		req, err := http.NewRequest(http.MethodPost, url, nil)
		if err == nil {
			req.Header.Set(requestIDHeader, correlationID)
			req.Header.Set(correlationIDHeader, correlationID)
			resp, requestErr := client.Do(req)
			if requestErr == nil {
				resp.Body.Close()
				if resp.StatusCode < 300 {
					appMetrics.Inc("pong_room_start_callback_success")
					return nil
				}
			}
		}
		if attempt < 2 {
			appMetrics.Inc("pong_room_start_callback_retry")
			time.Sleep(100 * time.Millisecond)
		}
	}
	appMetrics.Inc("pong_room_start_callback_failure")
	return errors.New("room start callback failed")
}

func notifyRoomFinished(roomID, lobbyAddr, correlationID string) {
	if lobbyAddr == "" {
		return
	}
	url := "http://" + lobbyAddr + "/internal/rooms/" + roomID + "/finished"
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		appMetrics.Inc("pong_room_finish_callback_failure")
		return
	}
	req.Header.Set(requestIDHeader, correlationID)
	req.Header.Set(correlationIDHeader, correlationID)
	resp, err := client.Do(req)
	if err != nil {
		appMetrics.Inc("pong_room_finish_callback_failure")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		appMetrics.Inc("pong_room_finish_callback_failure")
		return
	}
	appMetrics.Inc("pong_room_finish_callback_success")
}

func splitPath(path string) []string {
	var parts []string
	for _, p := range split(path, '/') {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func split(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			if i > start {
				parts = append(parts, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

// ---- HTTP policy and metrics ----

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func clientKey(r *http.Request) string {
	// NGINX overwrites X-Real-IP with the peer address. Do not trust the first
	// X-Forwarded-For value: a public caller can supply that header themselves.
	// Never log or export this key; it exists only in ephemeral limiter state.
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func tooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	http.Error(w, "too many requests", http.StatusTooManyRequests)
}

func lobbyWriteError(w http.ResponseWriter, err error) {
	status := lobby.RequestErrorStatus(err)
	if status >= 500 {
		http.Error(w, "internal server error", status)
		return
	}
	http.Error(w, err.Error(), status)
}

// publicAPIHandler applies a short-lived per-client concurrency bound. The
// endpoint-specific rate limiters are applied inside create/join handlers.
func publicAPIHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		release, ok := publicAdmission.AcquireHTTP(clientKey(r))
		if !ok {
			tooManyRequests(w)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware only permits the exact configured browser origins. Same-origin
// requests commonly omit Origin and remain valid; cross-origin requests that
// are not on the allowlist are rejected before reaching the API.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !websocketOrigins.allowed(origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}

type requestIDKey struct{}

const (
	requestIDHeader     = "X-Request-ID"
	correlationIDHeader = "X-Correlation-ID"
)

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failure is not a reason to expose request data. The fixed
		// fallback remains opaque and valid for the bounded correlation contract.
		return "00000000000000000000000000000000"
	}
	return fmt.Sprintf("%x", raw[:])
}

func validRequestID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func requestID(r *http.Request) string {
	if value := r.Context().Value(requestIDKey{}); value != nil {
		if id, ok := value.(string); ok {
			return id
		}
	}
	return "00000000000000000000000000000000"
}

func withRequestID(r *http.Request) *http.Request {
	inbound := strings.TrimSpace(r.Header.Get(requestIDHeader))
	if !validRequestID(inbound) {
		inbound = strings.TrimSpace(r.Header.Get(correlationIDHeader))
	}
	if !validRequestID(inbound) {
		inbound = newRequestID()
	}
	return r.WithContext(context.WithValue(r.Context(), requestIDKey{}, inbound))
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseRecorder) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("http hijacking is not supported")
	}
	w.status = http.StatusSwitchingProtocols
	return hj.Hijack()
}

func (w *responseRecorder) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func requestMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withRequestID(r)
		id := requestID(r)
		w.Header().Set(requestIDHeader, id)
		w.Header().Set(correlationIDHeader, id)
		recorder := &responseRecorder{ResponseWriter: w}
		appMetrics.Inc("pong_http_requests")
		next.ServeHTTP(recorder, r)
		switch status := recorder.statusCode(); {
		case status >= 500:
			appMetrics.Inc("pong_http_responses_5xx")
			appMetrics.Inc("pong_http_requests_failure")
		case status >= 400:
			appMetrics.Inc("pong_http_responses_4xx")
			appMetrics.Inc("pong_http_requests_failure")
		case status >= 300:
			appMetrics.Inc("pong_http_responses_3xx")
			appMetrics.Inc("pong_http_requests_success")
		case status >= 200:
			appMetrics.Inc("pong_http_responses_2xx")
			appMetrics.Inc("pong_http_requests_success")
		default:
			appMetrics.Inc("pong_http_responses_1xx")
			appMetrics.Inc("pong_http_requests_success")
		}
	})
}

// ---- WebSocket Proxy (lobby mode) ----

// proxyRoomWS relays WebSocket traffic between the client (via NGINX) and a
// dynamically created room pod.
//
// Architecture:
//
//	Browser --WS--> NGINX --HTTP+Upgrade--> Lobby (hijack) --WS--> Room Pod
//
// NGINX forwards the WebSocket upgrade request to the lobby. The lobby
// hijacks the raw client connection, sends back the 101 Switching Protocols
// response so NGINX enters tunnel mode, then opens a proper WebSocket
// connection to the room pod and relays validated frames between the two.
func proxyRoomWS(w http.ResponseWriter, r *http.Request, lobbySrv *lobby.Server, store *db.Store) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 3 || parts[2] != "ws" {
		http.NotFound(w, r)
		return
	}
	roomID := parts[1]
	if !lobby.ValidRoomID(roomID) {
		appMetrics.Inc("pong_websocket_proxy_rejected_invalid")
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}
	if !websocketOrigins.allowed(r.Header.Get("Origin")) {
		appMetrics.Inc("pong_websocket_proxy_rejected_origin")
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	release, ok := publicAdmission.AcquireWebSocket(clientKey(r))
	if !ok {
		appMetrics.Inc("pong_admission_websocket_rejected")
		tooManyRequests(w)
		return
	}
	defer release()
	appMetrics.AddGauge("pong_websockets_active", 1)
	defer appMetrics.AddGauge("pong_websockets_active", -1)

	addr, err := lobbySrv.GetRoomAddr(roomID)
	if err != nil {
		appMetrics.Inc("pong_websocket_proxy_room_not_found")
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Only accept WebSocket upgrade requests
	if r.Header.Get("Upgrade") != "websocket" {
		appMetrics.Inc("pong_websocket_proxy_rejected_upgrade")
		http.Error(w, "websocket only", http.StatusBadRequest)
		return
	}

	// ── Step 1: hijack the client connection (from NGINX) ──────────
	hj, ok := w.(http.Hijacker)
	if !ok {
		appMetrics.Inc("pong_websocket_proxy_hijack_failure")
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		appMetrics.Inc("pong_websocket_proxy_hijack_failure")
		return
	}
	defer clientConn.Close()
	enableTCPNoDelay(clientConn)

	// ── Step 2: open a real WebSocket connection to the room pod ───
	// Establish and read the first target message before acknowledging the
	// browser. This closes the gateway tunnel race: the room's immediate
	// "joined" frame is held in memory instead of arriving while the proxy
	// is still switching the client connection into tunnel mode.
	dialer := websocket.Dialer{
		HandshakeTimeout: 2 * time.Second,
		NetDialContext:   (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	}
	var target *websocket.Conn
	targetHeader := http.Header{}
	if origin := websocketOrigins.first(); origin != "" {
		targetHeader.Set("Origin", origin)
	}
	targetHeader.Set(requestIDHeader, requestID(r))
	targetHeader.Set(correlationIDHeader, requestID(r))
	for i := 0; i < 10; i++ {
		target, _, err = dialer.Dial("ws://"+addr+"/ws", targetHeader)
		if err == nil {
			break
		}
		appMetrics.Inc("pong_websocket_proxy_dial_retry")
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		appMetrics.Inc("pong_websocket_proxy_dial_failure")
		return
	}
	appMetrics.Inc("pong_websocket_proxy_dial_success")
	defer target.Close()
	enableTCPNoDelay(target.UnderlyingConn())
	target.SetReadLimit(maxProxyMessageSize)

	// ── Step 3: acknowledge the browser and start both relays ──────
	key := r.Header.Get("Sec-WebSocket-Key")
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	clientBuf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := clientBuf.Flush(); err != nil {
		appMetrics.Inc("pong_websocket_proxy_handshake_failure")
		return
	}

	// NGINX starts its tunnel copier after it receives the flushed 101. The
	// browser sends proxyReadyMessage from WebSocket.onopen; hold all target
	// frames until that post-upgrade client frame arrives. A timeout keeps raw
	// WebSocket clients that do not send an application frame working.
	clientReady := make(chan struct{})
	var readyOnce sync.Once
	signalClientReady := func() {
		readyOnce.Do(func() { close(clientReady) })
	}

	var clientWriteMu sync.Mutex
	writeClientFrame := func(messageType int, payload []byte) error {
		<-clientReady
		clientWriteMu.Lock()
		defer clientWriteMu.Unlock()
		return writeWebSocketFrame(clientConn, messageType, payload)
	}

	// Gorilla handles control frames from the room connection. Forwarding
	// them through the same gated writer preserves ordering with data frames.
	target.SetPingHandler(func(appData string) error {
		return writeClientFrame(websocket.PingMessage, []byte(appData))
	})
	target.SetPongHandler(func(appData string) error {
		return writeClientFrame(websocket.PongMessage, []byte(appData))
	})
	target.SetCloseHandler(func(code int, text string) error {
		closePayload := websocket.FormatCloseMessage(code, text)
		if err := target.WriteControl(websocket.CloseMessage, closePayload, time.Now().Add(5*time.Second)); err != nil && err != websocket.ErrCloseSent {
			return err
		}
		return writeClientFrame(websocket.CloseMessage, closePayload)
	})

	errCh := make(chan error, 2)
	go func() {
		errCh <- relayBrowserToTargetReady(clientBuf, target, signalClientReady)
	}()
	go func() {
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-clientReady:
		case <-timer.C:
			signalClientReady()
		}
		for {
			messageType, payload, e := target.ReadMessage()
			if e != nil {
				errCh <- e
				return
			}
			if e = writeClientFrame(messageType, payload); e != nil {
				errCh <- e
				return
			}
		}
	}()
	<-errCh
	appMetrics.Inc("pong_websocket_proxy_closed")
	appMetrics.Inc("pong_websocket_proxy_relay_ended")
}

const maxProxyMessageSize = 16 << 20
const proxyReadyMessage = `{"type":"proxy-ready"}`

// enableTCPNoDelay prevents the small, frequent game-state frames from being
// coalesced by Nagle's algorithm. This matters for the Kubernetes path, where
// each browser connection crosses the gateway and the lobby proxy before it
// reaches the room pod.
func enableTCPNoDelay(conn net.Conn) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	if err := tcpConn.SetNoDelay(true); err != nil {
		appMetrics.Inc("pong_websocket_tcp_nodelay_failure")
	}
}

// relayBrowserToTarget reads client-to-server WebSocket frames from the
// hijacked browser connection and sends equivalent messages through Gorilla.
func relayBrowserToTarget(r io.Reader, target *websocket.Conn) error {
	return relayBrowserToTargetReady(r, target, nil)
}

func relayBrowserToTargetReady(r io.Reader, target *websocket.Conn, ready func()) error {
	signalReady := func() {
		if ready != nil {
			ready()
		}
	}
	sendMessage := func(messageType int, payload []byte) error {
		signalReady()
		if messageType == websocket.TextMessage && bytes.Equal(payload, []byte(proxyReadyMessage)) {
			return nil
		}
		return target.WriteMessage(messageType, payload)
	}

	var fragmented bool
	var messageType int
	var message []byte

	for {
		fin, opcode, payload, err := readWebSocketFrame(r)
		if err != nil {
			return err
		}

		if opcode >= 0x8 {
			if !fin || len(payload) > 125 || opcode > byte(websocket.PongMessage) {
				return fmt.Errorf("invalid websocket control frame")
			}
			signalReady()
			if err := target.WriteControl(int(opcode), payload, time.Now().Add(5*time.Second)); err != nil {
				return err
			}
			continue
		}

		switch opcode {
		case 0x0: // continuation
			if !fragmented {
				return fmt.Errorf("unexpected websocket continuation frame")
			}
			if len(message)+len(payload) > maxProxyMessageSize {
				return fmt.Errorf("websocket message exceeds proxy limit")
			}
			message = append(message, payload...)
			if fin {
				if err := sendMessage(messageType, message); err != nil {
					return err
				}
				fragmented = false
				message = nil
			}
		case 0x1, 0x2: // text or binary
			if fragmented {
				return fmt.Errorf("new websocket data frame before continuation")
			}
			if len(payload) > maxProxyMessageSize {
				return fmt.Errorf("websocket message exceeds proxy limit")
			}
			if fin {
				if err := sendMessage(int(opcode), payload); err != nil {
					return err
				}
			} else {
				fragmented = true
				messageType = int(opcode)
				message = append(message[:0], payload...)
			}
		default:
			return fmt.Errorf("invalid websocket opcode: %d", opcode)
		}
	}
}

// readWebSocketFrame reads one frame from a client WebSocket. Client frames
// are required to be masked by RFC 6455; the mask is removed before delivery
// to Gorilla, which applies its own client-side masking when it writes.
func readWebSocketFrame(r io.Reader) (fin bool, opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err = io.ReadFull(r, header[:]); err != nil {
		return false, 0, nil, err
	}
	if header[0]&0x70 != 0 {
		return false, 0, nil, fmt.Errorf("websocket reserved bits are set")
	}

	fin = header[0]&0x80 != 0
	opcode = header[0] & 0x0f
	masked := header[1]&0x80 != 0
	if !masked {
		return false, 0, nil, fmt.Errorf("client websocket frame is not masked")
	}

	lengthCode := header[1] & 0x7f
	length := uint64(lengthCode)
	if lengthCode == 126 {
		var extended [2]byte
		if _, err = io.ReadFull(r, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
		if length < 126 {
			return false, 0, nil, fmt.Errorf("non-minimal websocket payload length")
		}
	} else if lengthCode == 127 {
		var extended [8]byte
		if _, err = io.ReadFull(r, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
		if length&(uint64(1)<<63) != 0 || length < 65536 {
			return false, 0, nil, fmt.Errorf("invalid websocket payload length")
		}
	}
	if opcode >= 0x8 {
		if opcode > byte(websocket.PongMessage) || !fin || length > 125 {
			return false, 0, nil, fmt.Errorf("invalid websocket control frame")
		}
	} else if length > maxProxyMessageSize {
		return false, 0, nil, fmt.Errorf("websocket frame exceeds proxy limit")
	}

	var mask [4]byte
	if _, err = io.ReadFull(r, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, int(length))
	if _, err = io.ReadFull(r, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, opcode, payload, nil
}

// writeWebSocketFrame writes an unmasked server-to-client WebSocket frame.
func writeWebSocketFrame(w io.Writer, messageType int, payload []byte) error {
	var header [10]byte
	header[0] = 0x80 | byte(messageType)
	n := 2
	switch {
	case len(payload) < 126:
		header[1] = byte(len(payload))
	case len(payload) <= 0xffff:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:4], uint16(len(payload)))
		n = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:10], uint64(len(payload)))
		n = 10
	}
	if err := writeAll(w, header[:n]); err != nil {
		return err
	}
	return writeAll(w, payload)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
