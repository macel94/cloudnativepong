package db

import (
	"strings"
	"sync"
	"testing"
)

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
