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
