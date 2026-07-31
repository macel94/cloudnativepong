package lobby

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudnativepong/db"
)

func newTestServer(t *testing.T, idleTimeout time.Duration) (*Server, *db.Store) {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	return NewServerWithIdleTimeout(store, "local", "", "", idleTimeout), store
}

func TestHandleCreateRoomReservesCreatorSlot(t *testing.T) {
	server, store := newTestServer(t, time.Minute)
	defer store.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/rooms/create", strings.NewReader(`{"name":"creator"}`))
	recorder := httptest.NewRecorder()
	server.HandleCreateRoom(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HandleCreateRoom() status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var room db.Room
	if err := json.Unmarshal(recorder.Body.Bytes(), &room); err != nil {
		t.Fatalf("decode room = %v", err)
	}
	if room.Players != 1 || room.Status != "waiting" {
		t.Fatalf("created room = %+v, want one waiting reservation", room)
	}
}

func TestJoinRoomLeavesWaitingUntilRoomStarts(t *testing.T) {
	server, store := newTestServer(t, time.Minute)
	defer store.Close()

	room, err := server.CreateRoom("test")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	if err := server.JoinRoom(room.ID); err != nil {
		t.Fatalf("first JoinRoom() error = %v", err)
	}
	got, err := store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if got.Players != 1 || got.Status != "waiting" {
		t.Fatalf("after first join room = %+v, want one waiting player", got)
	}

	if err := server.JoinRoom(room.ID); err != nil {
		t.Fatalf("second JoinRoom() error = %v", err)
	}
	got, err = store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() after second join error = %v", err)
	}
	if got.Status != "waiting" {
		t.Fatalf("after reservations status = %q, want waiting", got.Status)
	}
	if err := server.MarkRoomStarted(room.ID); err != nil {
		t.Fatalf("MarkRoomStarted() error = %v", err)
	}
	got, err = store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() after start error = %v", err)
	}
	if got.Status != "playing" {
		t.Fatalf("after start status = %q, want playing", got.Status)
	}
	if err := server.MarkRoomStarted(room.ID); err != nil {
		t.Fatalf("idempotent MarkRoomStarted() error = %v", err)
	}
}

func TestReconcileExpiresIdleWaitingRoom(t *testing.T) {
	server, store := newTestServer(t, time.Millisecond)
	defer store.Close()

	room, err := server.CreateRoom("abandoned")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := server.ReconcileRooms(); err != nil {
		t.Fatalf("ReconcileRooms() error = %v", err)
	}
	got, err := store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if got != nil {
		t.Fatalf("idle room still exists after reconciliation: %+v", got)
	}
}

func TestCleanupRoomIsIdempotent(t *testing.T) {
	server, store := newTestServer(t, time.Minute)
	defer store.Close()

	room, err := server.CreateRoom("cleanup")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	if err := server.CleanupRoom(room.ID); err != nil {
		t.Fatalf("first CleanupRoom() error = %v", err)
	}
	if err := server.CleanupRoom(room.ID); err != nil {
		t.Fatalf("second CleanupRoom() error = %v", err)
	}
	got, err := store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if got != nil {
		t.Fatalf("room still exists after cleanup: %+v", got)
	}
}
