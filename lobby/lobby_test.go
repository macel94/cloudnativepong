package lobby

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestRoomResourceRequirementsMatchCapacityContract(t *testing.T) {
	want := map[string]interface{}{
		"requests": map[string]string{
			"cpu":    "250m",
			"memory": "32Mi",
		},
		"limits": map[string]string{
			"memory": "64Mi",
		},
	}
	if got := roomResourceRequirements(); !reflect.DeepEqual(got, want) {
		t.Fatalf("roomResourceRequirements() = %#v, want %#v", got, want)
	}
}

func TestHandleCreateRoomReservesCreatorSlot(t *testing.T) {
	server, store := newTestServer(t, time.Minute)
	defer store.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/rooms/create", strings.NewReader(`{"name":"creator"}`))
	req.Header.Set("Content-Type", "application/json")
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

type fakeOrchestrator struct {
	createErr        error
	deletePodErr     error
	deleteServiceErr error
	pods             []RoomResource
	services         []RoomResource
	podDeletes       int
	serviceDeletes   int
}

func (f *fakeOrchestrator) CreateRoom(string) (string, error) { return "10.0.0.2", f.createErr }
func (f *fakeOrchestrator) DeletePod(string) error {
	f.podDeletes++
	return f.deletePodErr
}
func (f *fakeOrchestrator) DeleteService(string) error {
	f.serviceDeletes++
	return f.deleteServiceErr
}
func (f *fakeOrchestrator) ListPods() ([]RoomResource, error)     { return f.pods, nil }
func (f *fakeOrchestrator) ListServices() ([]RoomResource, error) { return f.services, nil }

func TestRoomEndpointSliceHasReadyAddress(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "ready address",
			body: `{"items":[{"endpoints":[{"addresses":["10.0.0.4"],"conditions":{"ready":true}}]}]}`,
			want: true,
		},
		{
			name: "not ready",
			body: `{"items":[{"endpoints":[{"addresses":["10.0.0.4"],"conditions":{"ready":false}}]}]}`,
			want: false,
		},
		{
			name: "missing ready condition",
			body: `{"items":[{"endpoints":[{"addresses":["10.0.0.4"]}]}]}`,
			want: true,
		},
		{
			name: "ready address in a later slice",
			body: `{"items":[{"endpoints":[{"addresses":["10.0.0.4"],"conditions":{"ready":false}}]},{"endpoints":[{"addresses":["10.0.0.5"],"conditions":{"ready":true}}]}]}`,
			want: true,
		},
		{
			name: "empty address",
			body: `{"items":[{"endpoints":[{"addresses":[],"conditions":{"ready":true}}]}]}`,
			want: false,
		},
		{
			name: "empty",
			body: `{"items":[]}`,
			want: false,
		},
		{
			name: "malformed",
			body: `{`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roomEndpointSliceHasReadyAddress([]byte(tt.body)); got != tt.want {
				t.Fatalf("roomEndpointSliceHasReadyAddress() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestK8sClientReusesTransport(t *testing.T) {
	first := k8sClient()
	second := k8sClient()
	if first != second {
		t.Fatal("k8sClient() returned different clients")
	}
	if first.Transport != second.Transport {
		t.Fatal("k8sClient() returned clients with different transports")
	}
}

func TestCreateRoomPodFailureRemovesDatabaseReservation(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()

	orchestrator := &fakeOrchestrator{createErr: errors.New("quota")}
	server := NewServerWithDependencies(store, "kubernetes", "", "", time.Minute, nil, orchestrator)
	if _, err := server.CreateRoom("quota"); err == nil {
		t.Fatal("CreateRoom() unexpectedly succeeded under injected quota failure")
	}
	rooms, err := store.ListRooms()
	if err != nil {
		t.Fatalf("ListRooms() error = %v", err)
	}
	if len(rooms) != 0 {
		t.Fatalf("rooms after failed Pod creation = %d, want 0", len(rooms))
	}
}

func TestCleanupRetainsRoomWhenResourceDeletionFailsThenRetries(t *testing.T) {
	server, store := newTestServer(t, time.Minute)
	defer store.Close()

	room, err := server.CreateRoom("cleanup")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	// Switch to the injected orchestration mode while retaining the persisted room.
	orchestrator := &fakeOrchestrator{deletePodErr: errors.New("pod unavailable")}
	server = NewServerWithDependencies(store, "kubernetes", "", "", time.Minute, nil, orchestrator)
	if err := server.CleanupRoom(room.ID); err == nil {
		t.Fatal("CleanupRoom() unexpectedly succeeded while Pod deletion failed")
	}
	got, err := store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if got == nil {
		t.Fatal("room was deleted despite incomplete resource cleanup")
	}
	orchestrator.deletePodErr = nil
	if err := server.CleanupRoom(room.ID); err != nil {
		t.Fatalf("retry CleanupRoom() error = %v", err)
	}
	if err := server.CleanupRoom(room.ID); err != nil {
		t.Fatalf("idempotent CleanupRoom() error = %v", err)
	}
}

func TestReconcileRemovesFailedPodAndOrphanServiceAfterRestart(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()
	orchestrator := &fakeOrchestrator{}
	first := NewServerWithDependencies(store, "kubernetes", "", "", time.Minute, nil, orchestrator)
	failed, err := first.CreateRoom("failed")
	if err != nil {
		t.Fatalf("CreateRoom(failed) error = %v", err)
	}
	orphan, err := first.CreateRoom("orphan")
	if err != nil {
		t.Fatalf("CreateRoom(orphan) error = %v", err)
	}

	// A fresh Server models an API restart: only persisted rows and cluster
	// observations are available to reconciliation.
	orchestrator.pods = []RoomResource{{RoomID: failed.ID, Phase: "Failed"}}
	orchestrator.services = []RoomResource{{RoomID: orphan.ID}}
	second := NewServerWithDependencies(store, "kubernetes", "", "", time.Minute, nil, orchestrator)
	if err := second.ReconcileRooms(); err != nil {
		t.Fatalf("ReconcileRooms() error = %v", err)
	}
	for _, id := range []string{failed.ID, orphan.ID} {
		got, err := store.GetRoom(id)
		if err != nil {
			t.Fatalf("GetRoom(%s) error = %v", id, err)
		}
		if got != nil {
			t.Fatalf("room %s survived reconciliation: %+v", id, got)
		}
	}
	if orchestrator.podDeletes == 0 || orchestrator.serviceDeletes == 0 {
		t.Fatalf("reconciliation delete counts = pods:%d services:%d, want both", orchestrator.podDeletes, orchestrator.serviceDeletes)
	}
}

func TestReconcileRemovesPlayingRoomWithoutLiveResources(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	defer store.Close()

	orchestrator := &fakeOrchestrator{}
	server := NewServerWithDependencies(store, "kubernetes", "", "", time.Minute, nil, orchestrator)
	room, err := server.CreateRoom("stale-playing")
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	if err := server.JoinRoom(room.ID); err != nil {
		t.Fatalf("first JoinRoom() error = %v", err)
	}
	if err := server.JoinRoom(room.ID); err != nil {
		t.Fatalf("second JoinRoom() error = %v", err)
	}
	if err := server.MarkRoomStarted(room.ID); err != nil {
		t.Fatalf("MarkRoomStarted() error = %v", err)
	}

	if err := server.ReconcileRooms(); err != nil {
		t.Fatalf("ReconcileRooms() error = %v", err)
	}
	got, err := store.GetRoom(room.ID)
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if got != nil {
		t.Fatalf("stale playing room survived reconciliation: %+v", got)
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
