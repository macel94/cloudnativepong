package db

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadsLegacySQLiteTimestampFormatAfterRestart(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()
	if _, err := store.db.Exec("INSERT INTO rooms (id, name, status, created_at) VALUES (?, ?, ?, ?)", "legacy", "old", "waiting", "2026-08-05T18:00:00Z"); err != nil {
		t.Fatalf("legacy insert error = %v", err)
	}
	room, err := store.GetRoom("legacy")
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if room == nil || !room.CreatedAt.Equal(time.Date(2026, 8, 5, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("legacy room timestamp = %+v", room)
	}
	rooms, err := store.ListRooms()
	if err != nil || len(rooms) != 1 {
		t.Fatalf("ListRooms() = %v, %v", rooms, err)
	}
}

func TestIncrementPlayersConcurrentCapacity(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	if _, err := store.CreateRoom("room", "test"); err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}

	const attempts = 12
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- store.IncrementPlayers("room")
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !strings.Contains(err.Error(), "room is full") {
			t.Errorf("IncrementPlayers() error = %v, want room is full after capacity", err)
		}
	}
	if successes != 2 {
		t.Fatalf("successful reservations = %d, want 2", successes)
	}

	room, err := store.GetRoom("room")
	if err != nil {
		t.Fatalf("GetRoom() error = %v", err)
	}
	if room.Players != 2 {
		t.Fatalf("stored players = %d, want 2", room.Players)
	}
	if room.Status != "waiting" {
		t.Fatalf("stored status = %q, want waiting until actual WebSockets start", room.Status)
	}
}

func TestMarkRoomPlayingRequiresTwoPlayersAndIsIdempotent(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer store.Close()

	if _, err := store.CreateRoom("room", "test"); err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	if err := store.MarkRoomPlaying("room"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("MarkRoomPlaying() with no players error = %v, want not ready", err)
	}
	if err := store.IncrementPlayers("room"); err != nil {
		t.Fatalf("first IncrementPlayers() error = %v", err)
	}
	if err := store.MarkRoomPlaying("room"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("MarkRoomPlaying() with one player error = %v, want not ready", err)
	}
	if err := store.IncrementPlayers("room"); err != nil {
		t.Fatalf("second IncrementPlayers() error = %v", err)
	}
	if err := store.MarkRoomPlaying("room"); err != nil {
		t.Fatalf("MarkRoomPlaying() error = %v", err)
	}
	if err := store.MarkRoomPlaying("room"); err != nil {
		t.Fatalf("idempotent MarkRoomPlaying() error = %v", err)
	}
}
