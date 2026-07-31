package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudnativepong/db"
	"github.com/cloudnativepong/lobby"
	"github.com/gorilla/websocket"
)

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
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(roomServer.URL, "http")+"/ws", nil)
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

func TestProxyRoomWSForwardsImmediateJoinedFrame(t *testing.T) {
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

	client, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(proxy.URL, "http")+"/rooms/"+room.ID+"/ws",
		nil,
	)
	if err != nil {
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
