package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudnativepong/db"
	"github.com/cloudnativepong/game"
	"github.com/cloudnativepong/lobby"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	mode := flag.String("mode", "local", "Mode: local, lobby, room")
	port := flag.String("port", "8080", "HTTP listen port")
	roomID := flag.String("room-id", "", "Room ID (for room mode)")
	lobbyAddr := flag.String("lobby-addr", "", "Lobby address (for room mode)")
	k8sNS := flag.String("namespace", "default", "Kubernetes namespace")
	roomImage := flag.String("room-image", "cloudnativepong-room:latest", "Room container image")
	flag.Parse()

	store, err := db.New(":memory:")
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer store.Close()

	lobbySrv := lobby.NewServer(store, *mode, *k8sNS, *roomImage)

	mux := http.NewServeMux()

	switch *mode {
	case "local":
		// In local mode, lobby and rooms share one process.
		// Rooms are spawned as goroutines and addressed via path /room/{id}/ws
		setupLocalRoutes(mux, lobbySrv, store)

	case "lobby":
		// Lobby-only mode: serves API, delegates rooms to K8s pods.
		// Static files are served by nginx.
		apiHandler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/rooms":
				lobbySrv.HandleListRooms(w, r)
			case "/api/rooms/create":
				lobbySrv.HandleCreateRoom(w, r)
			case "/api/rooms/join":
				lobbySrv.HandleJoinRoom(w, r)
			default:
				http.NotFound(w, r)
			}
		}))
		mux.Handle("/api/", apiHandler)
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

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// Graceful shutdown with timeout
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// ---- Local mode: lobby + rooms in one process ----

type localRoom struct {
	engine  *game.Engine
	players [2]*websocket.Conn
	mu      sync.Mutex
}

var localRooms sync.Map // roomID -> *localRoom

func setupLocalRoutes(mux *http.ServeMux, lobbySrv *lobby.Server, store *db.Store) {
	// API routes
	mux.HandleFunc("/api/rooms", lobbySrv.HandleListRooms)
	mux.HandleFunc("/api/rooms/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		room, err := lobbySrv.CreateRoom(req.Name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		// Register in-process handler
		handler := &lobby.RoomHandler{
			ID:   room.ID,
			Addr: fmt.Sprintf("localhost:%s/room/%s/ws", "8080", room.ID),
		}
		lobbySrv.RegisterLocalRoom(room.ID, handler)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(room)
	})
	mux.HandleFunc("/api/rooms/join", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			RoomID string `json:"room_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		if err := lobbySrv.JoinRoom(req.RoomID); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"room_id": req.RoomID,
			"mode":    "local",
		})
	})

	// Room WebSocket endpoint (both legacy and gateway paths)
	mux.HandleFunc("/room/", func(w http.ResponseWriter, r *http.Request) {
		// Parse /room/{id}/ws
		path := r.URL.Path
		parts := splitPath(path)
		if len(parts) < 3 || parts[2] != "ws" {
			http.NotFound(w, r)
			return
		}
		handleRoomWS(w, r, parts[1])
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
		handleRoomWS(w, r, parts[1])
	})

	// Static files (local mode only — served by nginx in K8s)
	mux.Handle("/", http.FileServer(http.Dir("./static")))
}

func setupRoomRoutes(mux *http.ServeMux, roomID, lobbyAddr string) {
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleRoomWS(w, r, roomID)
	})
	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
}

func handleRoomWS(w http.ResponseWriter, r *http.Request, roomID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer conn.Close()

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
		conn.WriteJSON(map[string]string{"type": "error", "message": "room full"})
		return
	}
	room.mu.Unlock()

	log.Printf("Player %d joined room %s", player, roomID)

	// Notify player of their assignment
	conn.WriteJSON(map[string]interface{}{
		"type":   "joined",
		"player": player,
	})

	// Mark ready
	room.engine.PlayerReady(player)

	// Read loop: handle input from this player
	go func() {
		for {
			var input game.Input
			err := conn.ReadJSON(&input)
			if err != nil {
				log.Printf("Player %d disconnected: %v", player, err)
				room.engine.PlayerLeft(player)
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

	// If this is player 2, start the game loop (or if already started, just listen)
	if player == 2 {
		// Player 2 joining triggers game start (both ready)
		room.engine.PlayerReady(2)
	}

	// Wait a tick for both players to be ready, then start the loop
	// Only one goroutine runs the tick loop
	if player == 2 {
		go runGameLoop(room)
	}

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
			return
		}
		if state.Status == game.StatusFinished {
			// Send final state and close
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
	})
	return v.(*localRoom)
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

// ---- CORS Middleware ----

// corsMiddleware wraps an http.Handler with CORS headers for API routes.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- WebSocket Proxy (lobby mode) ----

// proxyRoomWS - relays WebSocket between client (via Caddy) and room pod.
//
// Architecture:
//   Browser --WS--> Caddy --HTTP+Upgrade--> Lobby (hijack) --WS--> Room Pod
//
// Caddy forwards the WebSocket upgrade request to the lobby. The lobby
// hijacks the raw TCP connection, sends back the 101 Switching Protocols
// response (so Caddy enters tunnel mode), then opens a proper WebSocket
// connection to the room pod and relays frames between the two.
func proxyRoomWS(w http.ResponseWriter, r *http.Request, lobbySrv *lobby.Server, store *db.Store) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 3 || parts[2] != "ws" {
		http.NotFound(w, r)
		return
	}
	roomID := parts[1]

	addr, err := lobbySrv.GetRoomAddr(roomID)
	if err != nil {
		log.Printf("proxy: room %s not found: %v", roomID, err)
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	log.Printf("proxy: proxying WS for room %s to %s", roomID, addr)

	// Only accept WebSocket upgrade requests
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "websocket only", http.StatusBadRequest)
		return
	}

	// ── Step 1: hijack the client connection (from Caddy) ──────────
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		log.Printf("proxy: hijack error: %v", err)
		return
	}
	defer clientConn.Close()

	// ── Step 2: send 101 Switching Protocols (Caddy enters tunnel mode) ──
	key := r.Header.Get("Sec-WebSocket-Key")
	magic := "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.New()
	h.Write([]byte(key + magic))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	clientBuf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	clientBuf.Flush()

	// ── Step 3: open raw TCP to room pod and send WS upgrade ───────
	var targetConn net.Conn
	for i := 0; i < 10; i++ {
		targetConn, err = net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			break
		}
		log.Printf("proxy: retry %d connecting to %s: %v", i+1, addr, err)
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		log.Printf("proxy: failed to connect to %s after retries: %v", addr, err)
		return
	}
	defer targetConn.Close()

	// Write WebSocket upgrade request to room pod
	reqLine := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", addr, key)
	if _, err := targetConn.Write([]byte(reqLine)); err != nil {
		log.Printf("proxy: write upgrade to target: %v", err)
		return
	}

	// Read the 101 response from room pod (byte by byte to avoid
	// consuming the first WebSocket frame that may follow immediately)
	var respData []byte
	buf := make([]byte, 1)
	for {
		if _, err := targetConn.Read(buf); err != nil {
			log.Printf("proxy: read 101 from target: %v", err)
			return
		}
		respData = append(respData, buf[0])
		// Stop at end of HTTP headers (\r\n\r\n)
		if len(respData) >= 4 &&
			respData[len(respData)-4] == '\r' &&
			respData[len(respData)-3] == '\n' &&
			respData[len(respData)-2] == '\r' &&
			respData[len(respData)-1] == '\n' {
			break
		}
	}
	if !strings.Contains(string(respData), "101") {
		log.Printf("proxy: unexpected target response: %s", string(respData))
		return
	}

	// Forward any bytes already read after the 101 response (e.g. first WS frame)
	// to the browser connection. We do this by starting the io.Copy goroutines
	// and then writing any leftover bytes.
	// Actually, since we read byte-by-byte and stopped at \r\n\r\n, there should
	// be no leftover bytes. But to be safe, the bidirectional copy below handles this.

	// ── Step 4: bidirectional relay ────────────────────────────────
	errCh := make(chan error, 2)
	go func() {
		_, e := io.Copy(targetConn, clientConn)
		errCh <- e
	}()
	go func() {
		_, e := io.Copy(clientConn, targetConn)
		errCh <- e
	}()
	<-errCh
	log.Printf("proxy: WS connection closed for room %s", roomID)
}