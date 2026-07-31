package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	dbPath := flag.String("db-path", ":memory:", "SQLite database path")
	flag.Parse()

	store, err := db.New(*dbPath)
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
			switch {
			case r.URL.Path == "/api/rooms":
				lobbySrv.HandleListRooms(w, r)
			case r.URL.Path == "/api/rooms/create":
				lobbySrv.HandleCreateRoom(w, r)
			case r.URL.Path == "/api/rooms/join":
				lobbySrv.HandleJoinRoom(w, r)
			default:
				http.NotFound(w, r)
			}
		}))
		mux.Handle("/api/", apiHandler)
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
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

	srv := &http.Server{Addr: addr, Handler: mux}
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
			notifyRoomFinished(*roomID, *lobbyAddr)
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
	engine   *game.Engine
	players  [2]*websocket.Conn
	mu       sync.Mutex
	loopOnce sync.Once
	done     chan struct{}
	doneOnce sync.Once
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
		// Room creation reserves the creator's player slot; the browser opens
		// the corresponding WebSocket after receiving this response.
		if err := lobbySrv.JoinRoom(room.ID); err != nil {
			_ = lobbySrv.CleanupRoom(room.ID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		room.Players = 1

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
	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
}

func handleRoomWS(w http.ResponseWriter, r *http.Request, roomID, lobbyAddr string) {
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

	// Notify player of their assignment.
	if err := conn.WriteJSON(map[string]interface{}{
		"type":   "joined",
		"player": player,
	}); err != nil {
		room.signalFinished()
		return
	}

	// Mark ready. The lobby's playing transition is deliberately based on
	// actual WebSocket connections, not only on API reservations.
	room.engine.PlayerReady(player)
	if player == 2 {
		if err := notifyRoomStarted(roomID, lobbyAddr); err != nil {
			// Do not leave a live game in waiting state: reconciliation would
			// eventually expire it as an abandoned room. The room process will
			// report finished and the lobby will clean up its resources.
			log.Printf("room %s: failed to notify lobby that it started: %v", roomID, err)
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
				log.Printf("Player %d disconnected: %v", player, err)
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
			room.signalFinished()
			return
		}
		if state.Status == game.StatusFinished {
			// Send final state and close. The room-mode process also watches the
			// room completion signal and reports it to the lobby.
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

func (r *localRoom) signalFinished() {
	r.doneOnce.Do(func() { close(r.done) })
}

const roomReconcileInterval = time.Minute

func reconcileLobbyRooms(lobbySrv *lobby.Server) {
	if err := lobbySrv.ReconcileRooms(); err != nil {
		log.Printf("room reconciliation: %v", err)
	}
	ticker := time.NewTicker(roomReconcileInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := lobbySrv.ReconcileRooms(); err != nil {
			log.Printf("room reconciliation: %v", err)
		}
	}
}

func notifyRoomStarted(roomID, lobbyAddr string) error {
	if lobbyAddr == "" {
		return nil
	}
	url := "http://" + lobbyAddr + "/internal/rooms/" + roomID + "/started"
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := client.Post(url, "application/json", nil)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("lobby start notification returned HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if attempt < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return lastErr
}

func notifyRoomFinished(roomID, lobbyAddr string) {
	if lobbyAddr == "" {
		return
	}
	url := "http://" + lobbyAddr + "/internal/rooms/" + roomID + "/finished"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", nil)
	if err != nil {
		log.Printf("room %s: failed to notify lobby: %v", roomID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("room %s: lobby cleanup returned HTTP %d", roomID, resp.StatusCode)
	}
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

	// ── Step 1: hijack the client connection (from NGINX) ──────────
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
	for i := 0; i < 10; i++ {
		target, _, err = dialer.Dial("ws://"+addr+"/ws", nil)
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
	defer target.Close()
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
		log.Printf("proxy: flush client handshake: %v", err)
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
	log.Printf("proxy: WS connection closed for room %s", roomID)
}

const maxProxyMessageSize = 16 << 20
const proxyReadyMessage = `{"type":"proxy-ready"}`

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
