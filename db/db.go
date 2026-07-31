// Package db provides a lightweight SQLite-backed store for room state.
// Uses modernc.org/sqlite — pure Go, no CGO, works on scratch containers.
package db

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Room represents a game room stored in the database.
type Room struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"` // "waiting", "playing", "finished"
	PodIP     string    `json:"pod_ip"`
	Players   int       `json:"players"`
	CreatedAt time.Time `json:"created_at"`
}

// Store wraps a SQLite connection for room persistence.
type Store struct {
	mu sync.RWMutex
	db *sql.DB
}

// New opens (or creates) the SQLite database at the given path and runs migrations.
// Use ":memory:" for an in-memory database.
func New(path string) (*Store, error) {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	d.SetMaxOpenConns(1) // SQLite serializes writes anyway
	s := &Store{db: d}
	if err := s.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS rooms (
			id        TEXT PRIMARY KEY,
			name      TEXT NOT NULL DEFAULT '',
			status    TEXT NOT NULL DEFAULT 'waiting',
			pod_ip    TEXT NOT NULL DEFAULT '',
			players   INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
	return err
}

// CreateRoom inserts a new room and returns it.
func (s *Store) CreateRoom(id, name string) (*Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Room{ID: id, Name: name, Status: "waiting", Players: 0, CreatedAt: time.Now().UTC()}
	_, err := s.db.Exec(
		"INSERT INTO rooms (id, name, status, pod_ip, players, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		r.ID, r.Name, r.Status, r.PodIP, r.Players, r.CreatedAt.Format("2006-01-02T15:04:05Z"),
	)
	if err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	return r, nil
}

// GetRoom returns a room by ID, or nil if not found.
func (s *Store) GetRoom(id string) (*Room, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row := s.db.QueryRow("SELECT id, name, status, pod_ip, players, created_at FROM rooms WHERE id = ?", id)
	r := &Room{}
	var created string
	err := row.Scan(&r.ID, &r.Name, &r.Status, &r.PodIP, &r.Players, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt, _ = time.Parse("2006-01-02T15:04:05Z", created)
	return r, nil
}

// ListRooms returns all rooms that are not finished.
func (s *Store) ListRooms() ([]Room, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT id, name, status, pod_ip, players, created_at FROM rooms WHERE status != 'finished' ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []Room
	for rows.Next() {
		var r Room
		var created string
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.PodIP, &r.Players, &created); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse("2006-01-02T15:04:05Z", created)
		rooms = append(rooms, r)
	}
	return rooms, nil
}

// UpdateRoomStatus updates a room's status and pod IP.
func (s *Store) UpdateRoomStatus(id, status, podIP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE rooms SET status = ?, pod_ip = ? WHERE id = ?", status, podIP, id)
	return err
}

// IncrementPlayers atomically increments the player count without allowing a
// room to exceed its two-player capacity.
func (s *Store) IncrementPlayers(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("UPDATE rooms SET players = players + 1 WHERE id = ? AND players < 2", id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 1 {
		return nil
	}

	var exists bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id = ?)", id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("room not found")
	}
	return fmt.Errorf("room is full")
}

// MarkRoomPlaying records that both reserved players have actually connected
// to the room WebSocket. It is idempotent for an already-playing room and does
// not transition a room until its two-player reservation is complete.
func (s *Store) MarkRoomPlaying(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(
		"UPDATE rooms SET status = 'playing' WHERE id = ? AND status = 'waiting' AND players = 2",
		id,
	)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 1 {
		return nil
	}

	var status string
	var players int
	err = s.db.QueryRow("SELECT status, players FROM rooms WHERE id = ?", id).Scan(&status, &players)
	if err == sql.ErrNoRows {
		return fmt.Errorf("room not found")
	}
	if err != nil {
		return err
	}
	if status == "playing" {
		return nil
	}
	if status == "finished" {
		return fmt.Errorf("room is finished")
	}
	return fmt.Errorf("room is not ready: %d/2 players", players)
}

// DeleteRoom removes a room from the database.
func (s *Store) DeleteRoom(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM rooms WHERE id = ?", id)
	return err
}
