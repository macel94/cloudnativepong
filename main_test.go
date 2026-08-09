package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudnativepong/admission"
	"github.com/cloudnativepong/db"
	"github.com/cloudnativepong/lobby"
	"github.com/cloudnativepong/metrics"
	"github.com/gorilla/websocket"
)

func TestRequestIDIsOpaqueValidatedAndPropagated(t *testing.T) {
	for i := 0; i < 3; i++ {
		id := newRequestID()
		if !validRequestID(id) {
			t.Fatalf("newRequestID() = %q, want lowercase 32-hex ID", id)
		}
	}
	for _, value := range []string{"", "room-123", strings.Repeat("a", 31), strings.Repeat("A", 32), strings.Repeat("a", 33)} {
		if validRequestID(value) {
			t.Fatalf("validRequestID(%q) unexpectedly accepted", value)
		}
	}

	registry := metrics.NewRegistry()
	old := appMetrics
	appMetrics = registry
	defer func() { appMetrics = old }()
	handler := requestMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := requestID(r); got != strings.Repeat("b", 32) {
			t.Fatalf("requestID() = %q, want validated inbound ID", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(requestIDHeader, strings.Repeat("b", 32))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Header().Get(requestIDHeader) != strings.Repeat("b", 32) || recorder.Header().Get(correlationIDHeader) != strings.Repeat("b", 32) {
		t.Fatalf("request correlation headers = %q/%q", recorder.Header().Get(requestIDHeader), recorder.Header().Get(correlationIDHeader))
	}
	counters, _ := registry.Snapshot()
	if counters["pong_http_requests_success"] != 1 || counters["pong_http_responses_2xx"] != 1 {
		t.Fatalf("success metrics = %+v", counters)
	}
	body := registry.Render()
	if !strings.Contains(body, "pong_http_request_duration_seconds_count 1") {
		t.Fatalf("request duration metric missing: %q", body)
	}
}

func TestRequestMetricsCountsFailureWithoutRequestData(t *testing.T) {
	registry := metrics.NewRegistry()
	old := appMetrics
	appMetrics = registry
	defer func() { appMetrics = old }()
	handler := requestMetrics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/bad", nil))
	if recorder.Code != http.StatusBadRequest || !validRequestID(recorder.Header().Get(requestIDHeader)) {
		t.Fatalf("response = %d with request ID %q", recorder.Code, recorder.Header().Get(requestIDHeader))
	}
	counters, _ := registry.Snapshot()
	if counters["pong_http_requests_failure"] != 1 || counters["pong_http_responses_4xx"] != 1 {
		t.Fatalf("failure metrics = %+v", counters)
	}
}

func TestCapabilitiesAdvertiseConfiguredWebTransport(t *testing.T) {
	oldEnabled, oldURL := webTransportEnabled, webTransportURL
	webTransportEnabled = true
	webTransportURL = "https://pong.example/rooms/{room}/wt"
	defer func() {
		webTransportEnabled, webTransportURL = oldEnabled, oldURL
	}()

	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()
	mux := http.NewServeMux()
	setupLocalRoutes(mux, lobby.NewServer(store, "local", "", ""), store)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d, want 200", recorder.Code)
	}
	var got struct {
		WebTransport    bool   `json:"webtransport"`
		WebTransportURL string `json:"webtransport_url"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if !got.WebTransport || got.WebTransportURL != webTransportURL {
		t.Fatalf("capabilities = %+v, want enabled URL %q", got, webTransportURL)
	}
}

func TestOriginAllowlistIsExactAndNormalized(t *testing.T) {
	allowlist := loadOriginAllowlist("lobby", "https://PONG.BELACCA.COM/")
	if !allowlist.allowed("https://pong.belacca.com") {
		t.Fatal("normalized production origin should be allowed")
	}
	for _, origin := range []string{"https://evil.example", "https://pong.belacca.com/path", "https://pong.belacca.com?x=1", ""} {
		if allowlist.allowed(origin) {
			t.Fatalf("origin %q should be rejected", origin)
		}
	}
}

func TestNotifyRoomStartedFailureIsBoundedAndMeasured(t *testing.T) {
	registry := metrics.NewRegistry()
	old := appMetrics
	appMetrics = registry
	defer func() { appMetrics = old }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if err := notifyRoomStarted("a1b2c3", strings.TrimPrefix(server.URL, "http://"), strings.Repeat("d", 32)); err == nil {
		t.Fatal("notifyRoomStarted() unexpectedly succeeded")
	}
	counters, _ := registry.Snapshot()
	if counters["pong_room_start_callback_attempt"] != 3 || counters["pong_room_start_callback_retry"] != 2 || counters["pong_room_start_callback_failure"] != 1 {
		t.Fatalf("callback failure metrics = %+v", counters)
	}
}

func TestNotifyRoomStartedForwardsOnlyCorrelationID(t *testing.T) {
	seen := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if err := notifyRoomStarted("a1b2c3", strings.TrimPrefix(server.URL, "http://"), strings.Repeat("c", 32)); err != nil {
		t.Fatalf("notifyRoomStarted() error = %v", err)
	}
	select {
	case headers := <-seen:
		if headers.Get(requestIDHeader) != strings.Repeat("c", 32) || headers.Get(correlationIDHeader) != strings.Repeat("c", 32) {
			t.Fatalf("callback correlation headers = %q/%q", headers.Get(requestIDHeader), headers.Get(correlationIDHeader))
		}
		if headers.Get("X-Forwarded-For") != "" {
			t.Fatal("callback unexpectedly forwarded client address")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for callback")
	}
}

func TestLocalCreateAdmissionRejectionIsMeasured(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()
	oldAdmission := publicAdmission
	oldMetrics := appMetrics
	publicAdmission = admission.NewController(admission.Config{
		Window:              time.Minute,
		CreatePerWindow:     1,
		JoinPerWindow:       2,
		HTTPPerClient:       4,
		WebSocketsPerClient: 1,
		MaxWebSockets:       2,
		MaxClients:          4,
	})
	appMetrics = metrics.NewRegistry()
	defer func() {
		publicAdmission = oldAdmission
		appMetrics = oldMetrics
	}()
	server := lobby.NewServer(store, "local", "", "")
	mux := http.NewServeMux()
	setupLocalRoutes(mux, server, store)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/rooms/create", strings.NewReader(`{"name":"smoke"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.10:1234"
		recorder := httptest.NewRecorder()
		requestMetrics(mux).ServeHTTP(recorder, req)
		return recorder
	}
	if got := request(); got.Code != http.StatusOK {
		t.Fatalf("first create status = %d, want 200", got.Code)
	}
	if got := request(); got.Code != http.StatusTooManyRequests {
		t.Fatalf("second create status = %d, want 429", got.Code)
	}
	counters, _ := appMetrics.Snapshot()
	if counters["pong_admission_create_rejected"] != 1 || counters["pong_http_responses_4xx"] == 0 {
		t.Fatalf("admission metrics = %+v", counters)
	}
}

func TestClientKeyDoesNotTrustSpoofableForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.8:4321"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientKey(req); got != "10.0.0.8" {
		t.Fatalf("clientKey() = %q, want socket peer", got)
	}
	req.Header.Set("X-Real-IP", "203.0.113.10")
	if got := clientKey(req); got != "203.0.113.10" {
		t.Fatalf("clientKey() = %q, want trusted gateway address", got)
	}
}

func TestRoomTemplateHasSecurityBoundary(t *testing.T) {
	contents, err := os.ReadFile("k8s/overlays/server/room-template.yaml")
	if err != nil {
		t.Fatalf("read room template: %v", err)
	}
	text := string(contents)
	start := strings.Index(text, "    {")
	if start < 0 {
		t.Fatal("room template JSON not found")
	}
	var manifest struct {
		Spec struct {
			AutomountServiceAccountToken bool `json:"automountServiceAccountToken"`
			SecurityContext              struct {
				RunAsNonRoot   bool `json:"runAsNonRoot"`
				SeccompProfile struct {
					Type string `json:"type"`
				} `json:"seccompProfile"`
			} `json:"securityContext"`
			Containers []struct {
				SecurityContext struct {
					AllowPrivilegeEscalation bool `json:"allowPrivilegeEscalation"`
					ReadOnlyRootFilesystem   bool `json:"readOnlyRootFilesystem"`
					RunAsNonRoot             bool `json:"runAsNonRoot"`
				} `json:"securityContext"`
			} `json:"containers"`
		} `json:"spec"`
	}
	jsonText := strings.TrimSpace(text[start:])
	jsonText = jsonText[:strings.LastIndex(jsonText, "\n    }")+6]
	if err := json.Unmarshal([]byte(jsonText), &manifest); err != nil {
		t.Fatalf("decode room template JSON: %v", err)
	}
	if manifest.Spec.AutomountServiceAccountToken || !manifest.Spec.SecurityContext.RunAsNonRoot || manifest.Spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("pod security context is incomplete: %+v", manifest.Spec)
	}
	if len(manifest.Spec.Containers) != 1 || manifest.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation || !manifest.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem || !manifest.Spec.Containers[0].SecurityContext.RunAsNonRoot {
		t.Fatalf("container security context is incomplete: %+v", manifest.Spec.Containers)
	}
	if !strings.Contains(jsonText, `"name": "PONG_ALLOWED_ORIGINS"`) || !strings.Contains(jsonText, `"value": "https://pong.belacca.com"`) {
		t.Fatal("room Pod template does not propagate the production PONG_ALLOWED_ORIGINS policy")
	}
}

func TestWebTransportJSONRoundTrip(t *testing.T) {
	var encoded bytes.Buffer
	want := map[string]interface{}{"type": "input", "player": float64(2), "up": true}
	if err := writeWebTransportJSON(&encoded, want); err != nil {
		t.Fatalf("writeWebTransportJSON() error = %v", err)
	}
	var got map[string]interface{}
	if err := readWebTransportJSON(&encoded, &got); err != nil {
		t.Fatalf("readWebTransportJSON() error = %v", err)
	}
	if !jsonEqual(got, want) {
		t.Fatalf("round-trip value = %#v, want %#v", got, want)
	}
}

func TestWebTransportJSONRejectsOversizedFrame(t *testing.T) {
	var encoded bytes.Buffer
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], maxWebTransportMessageSize+1)
	encoded.Write(length[:])
	var got map[string]interface{}
	if err := readWebTransportJSON(&encoded, &got); err == nil {
		t.Fatal("readWebTransportJSON() accepted an oversized frame")
	}
	if err := writeWebTransportJSON(&encoded, bytes.Repeat([]byte{'x'}, maxWebTransportMessageSize+1)); err == nil {
		t.Fatal("writeWebTransportJSON() accepted an oversized value")
	}
}

func jsonEqual(left, right interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func TestReadWebSocketFrame(t *testing.T) {
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	input := clientFrame(true, 0x1, []byte("hello"), mask)

	fin, opcode, payload, err := readWebSocketFrame(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("readWebSocketFrame() error = %v", err)
	}
	if !fin || opcode != 0x1 || string(payload) != "hello" {
		t.Fatalf("decoded frame = fin:%v opcode:%d payload:%q", fin, opcode, payload)
	}
}

func TestReadWebSocketFrameRejectsInvalidFrames(t *testing.T) {
	mask := [4]byte{1, 2, 3, 4}
	tests := []struct {
		name  string
		frame []byte
	}{
		{
			name:  "unmasked",
			frame: []byte{0x81, 0x01, 'x'},
		},
		{
			name:  "reserved bits",
			frame: clientFrameWithHeader(0xc1, []byte("x"), mask),
		},
		{
			name:  "fragmented ping",
			frame: clientFrameWithHeader(0x09, []byte{}, mask),
		},
		{
			name:  "invalid control opcode",
			frame: clientFrameWithHeader(0x8b, []byte{}, mask),
		},
		{
			name:  "non-minimal 126 length",
			frame: append([]byte{0x81, 0xfe, 0, 1}, mask[:]...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := readWebSocketFrame(bytes.NewReader(tt.frame)); err == nil {
				t.Fatal("readWebSocketFrame() unexpectedly accepted invalid frame")
			}
		})
	}
}

func TestWriteWebSocketFramePayloadLengths(t *testing.T) {
	for _, size := range []int{0, 1, 125, 126, 65535, 65536} {
		t.Run(strings.TrimSpace(strings.ReplaceAll(formatInt(size), " ", "")), func(t *testing.T) {
			payload := bytes.Repeat([]byte{'p'}, size)
			var encoded bytes.Buffer
			if err := writeWebSocketFrame(&encoded, websocket.BinaryMessage, payload); err != nil {
				t.Fatalf("writeWebSocketFrame() error = %v", err)
			}

			decoded, err := decodeServerFrame(encoded.Bytes())
			if err != nil {
				t.Fatalf("decodeServerFrame() error = %v", err)
			}
			if decoded.opcode != websocket.BinaryMessage || !decoded.fin || decoded.masked {
				t.Fatalf("decoded header = %+v", decoded)
			}
			if !bytes.Equal(decoded.payload, payload) {
				t.Fatalf("decoded payload length = %d, want %d", len(decoded.payload), size)
			}
		})
	}
}

func TestRoomWebSocketsNotifyLobbyWhenBothPlayersConnect(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()

	lobbySrv := lobby.NewServer(store, "local", "", "")
	room, err := lobbySrv.CreateRoom("actual-start")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	if err := lobbySrv.JoinRoom(room.ID); err != nil {
		t.Fatalf("first JoinRoom() error = %v", err)
	}
	if err := lobbySrv.JoinRoom(room.ID); err != nil {
		t.Fatalf("second JoinRoom() error = %v", err)
	}

	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := splitPath(r.URL.Path)
		if len(parts) != 4 || parts[3] != "started" {
			http.NotFound(w, r)
			return
		}
		lobbySrv.HandleRoomStarted(w, r, parts[2])
	}))
	defer callback.Close()

	mux := http.NewServeMux()
	setupRoomRoutes(mux, room.ID, strings.TrimPrefix(callback.URL, "http://"))
	roomServer := httptest.NewServer(mux)
	defer roomServer.Close()

	dial := func() *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(roomServer.URL, "http")+"/ws", http.Header{"Origin": []string{"http://localhost:8080"}})
		if err != nil {
			t.Fatalf("room WebSocket dial error = %v", err)
		}
		return conn
	}
	player1 := dial()
	defer player1.Close()
	if _, _, err := player1.ReadMessage(); err != nil {
		t.Fatalf("player 1 joined message error = %v", err)
	}

	got, err := store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() after player 1 error = %v", err)
	}
	if got.Status != "waiting" {
		t.Fatalf("status after player 1 = %q, want waiting", got.Status)
	}

	player2 := dial()
	defer player2.Close()
	if _, _, err := player2.ReadMessage(); err != nil {
		t.Fatalf("player 2 joined message error = %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		got, err = store.GetRoom(room.ID)
		if err != nil {
			t.Fatalf("GetRoom() after player 2 error = %v", err)
		}
		if got.Status == "playing" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status after player 2 = %q, want playing", got.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRoomWebSocketStartCallbackFailureDoesNotMarkPlaying(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()
	lobbySrv := lobby.NewServer(store, "local", "", "")
	room, err := lobbySrv.CreateRoom("callback-failure")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	if err := lobbySrv.JoinRoom(room.ID); err != nil {
		t.Fatalf("first JoinRoom() error = %v", err)
	}
	if err := lobbySrv.JoinRoom(room.ID); err != nil {
		t.Fatalf("second JoinRoom() error = %v", err)
	}
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer callback.Close()

	mux := http.NewServeMux()
	setupRoomRoutes(mux, room.ID, strings.TrimPrefix(callback.URL, "http://"))
	roomServer := httptest.NewServer(mux)
	defer roomServer.Close()
	dial := func() *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(roomServer.URL, "http")+"/ws", http.Header{"Origin": []string{"http://localhost:8080"}})
		if err != nil {
			t.Fatalf("room WebSocket dial error = %v", err)
		}
		return conn
	}
	first := dial()
	defer first.Close()
	first.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := first.ReadMessage(); err != nil {
		t.Fatalf("first joined message error = %v", err)
	}
	second := dial()
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := second.ReadMessage(); err != nil {
		// The joined frame is written before the callback; a close is also an
		// acceptable failure boundary if the callback fails immediately.
	}
	got, err := store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if got == nil || got.Status == "playing" {
		t.Fatalf("room after callback failure = %+v, want retained and not playing", got)
	}
	_ = first.Close()
	_ = second.Close()
}

func TestProxyRoomWSForwardsImmediateJoinedFrame(t *testing.T) {
	oldAdmission := publicAdmission
	publicAdmission = admission.NewController(admission.Config{
		Window:              time.Minute,
		CreatePerWindow:     100,
		JoinPerWindow:       100,
		HTTPPerClient:       32,
		WebSocketsPerClient: 32,
		MaxWebSockets:       128,
		MaxClients:          64,
	})
	t.Cleanup(func() { publicAdmission = oldAdmission })

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Send the first application frame immediately after the target 101.
		// This is the ordering that previously exposed the lost-frame bug.
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"joined","player":1}`)); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer target.Close()

	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()

	lobbySrv := lobby.NewServer(store, "local", "", "")
	room, err := lobbySrv.CreateRoom("immediate-joined")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	lobbySrv.RegisterLocalRoom(room.ID, &lobby.RoomHandler{
		ID:   room.ID,
		Addr: strings.TrimPrefix(target.URL, "http://"),
	})

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRoomWS(w, r, lobbySrv, store)
	}))
	defer proxy.Close()

	client, response, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(proxy.URL, "http")+"/rooms/"+room.ID+"/ws",
		http.Header{"Origin": []string{"http://localhost:8080"}},
	)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("proxy Dial() error = %v, HTTP %d: %s", err, response.StatusCode, body)
		}
		t.Fatalf("proxy Dial() error = %v", err)
	}
	defer client.Close()

	if err := client.WriteJSON(map[string]string{"type": "proxy-ready"}); err != nil {
		t.Fatalf("WriteJSON(proxy-ready) error = %v", err)
	}

	messageType, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if messageType != websocket.TextMessage || string(payload) != `{"type":"joined","player":1}` {
		t.Fatalf("received message type=%d payload=%s, want immediate joined frame", messageType, payload)
	}
}

func TestRelayBrowserToTargetSuppressesProxyReady(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err == nil && messageType == websocket.TextMessage {
			received <- string(payload)
		}
	}))
	defer server.Close()

	target, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer target.Close()

	mask := [4]byte{5, 6, 7, 8}
	input := append(
		clientFrame(true, 0x1, []byte(proxyReadyMessage), mask),
		clientFrame(true, 0x1, []byte(`{"player":1}`), mask)...,
	)
	ready := false
	if err := relayBrowserToTargetReady(bytes.NewReader(input), target, func() { ready = true }); !errors.Is(err, io.EOF) {
		t.Fatalf("relayBrowserToTargetReady() error = %v, want io.EOF", err)
	}
	if !ready {
		t.Fatal("relayBrowserToTargetReady() did not signal readiness")
	}

	select {
	case got := <-received:
		if got != `{"player":1}` {
			t.Fatalf("server received %q, want the real client message", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for real client message")
	}
}

func TestRelayBrowserToTargetReassemblesFragments(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err == nil && messageType == websocket.TextMessage {
			received <- string(payload)
		}
	}))
	defer server.Close()

	target, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer target.Close()

	mask := [4]byte{9, 8, 7, 6}
	input := append(
		clientFrame(false, 0x1, []byte("hello "), mask),
		clientFrame(true, 0x0, []byte("world"), mask)...,
	)
	if err := relayBrowserToTarget(bytes.NewReader(input), target); !errors.Is(err, io.EOF) {
		t.Fatalf("relayBrowserToTarget() error = %v, want io.EOF", err)
	}

	select {
	case got := <-received:
		if got != "hello world" {
			t.Fatalf("server received %q, want %q", got, "hello world")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reassembled message")
	}
}

type decodedFrame struct {
	fin     bool
	opcode  byte
	masked  bool
	payload []byte
}

func decodeServerFrame(frame []byte) (decodedFrame, error) {
	if len(frame) < 2 {
		return decodedFrame{}, io.ErrUnexpectedEOF
	}
	result := decodedFrame{
		fin:    frame[0]&0x80 != 0,
		opcode: frame[0] & 0x0f,
		masked: frame[1]&0x80 != 0,
	}
	length := int(frame[1] & 0x7f)
	pos := 2
	switch length {
	case 126:
		if len(frame) < pos+2 {
			return decodedFrame{}, io.ErrUnexpectedEOF
		}
		length = int(frame[pos])<<8 | int(frame[pos+1])
		pos += 2
	case 127:
		if len(frame) < pos+8 {
			return decodedFrame{}, io.ErrUnexpectedEOF
		}
		wideLength := binary.BigEndian.Uint64(frame[pos : pos+8])
		if wideLength > uint64(len(frame)-(pos+8)) {
			return decodedFrame{}, io.ErrUnexpectedEOF
		}
		length = int(wideLength)
		pos += 8
	}
	if len(frame) < pos+length {
		return decodedFrame{}, io.ErrUnexpectedEOF
	}
	result.payload = frame[pos : pos+length]
	return result, nil
}

func clientFrame(fin bool, opcode byte, payload []byte, mask [4]byte) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	return clientFrameWithHeader(first, payload, mask)
}

func clientFrameWithHeader(first byte, payload []byte, mask [4]byte) []byte {
	var frame bytes.Buffer
	frame.WriteByte(first)
	switch {
	case len(payload) < 126:
		frame.WriteByte(0x80 | byte(len(payload)))
	case len(payload) <= 0xffff:
		frame.WriteByte(0x80 | 126)
		frame.WriteByte(byte(len(payload) >> 8))
		frame.WriteByte(byte(len(payload)))
	default:
		frame.WriteByte(0x80 | 127)
		for shift := uint(56); ; shift -= 8 {
			frame.WriteByte(byte(uint64(len(payload)) >> shift))
			if shift == 0 {
				break
			}
		}
	}
	frame.Write(mask[:])
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	frame.Write(masked)
	return frame.Bytes()
}

func formatInt(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}
