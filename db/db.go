// Package db provides a lightweight SQLite-backed store for room state.
// Uses modernc.org/sqlite — pure Go, no CGO, works in distroless containers.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Recorder is the small metrics surface needed by the store. Keeping this
// interface local avoids coupling persistence to a particular exposition
// implementation while ensuring all operation names remain code-defined.
type Recorder interface {
	Inc(string)
	AddGauge(string, int64)
	SetGauge(string, int64)
}

var (
	ErrRoomNotFound = errors.New("room not found")
	ErrRoomFull     = errors.New("room is full")
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
	mu       sync.RWMutex
	db       *sql.DB
	recorder Recorder
}

// New opens (or creates) the SQLite database at the given path and runs migrations.
// Use ":memory:" for an in-memory database.
func New(path string) (*Store, error) { return NewWithMetrics(path, nil) }

// NewWithMetrics is New with an optional application metrics recorder.
func NewWithMetrics(path string, recorder Recorder) (*Store, error) {
	s := &Store{recorder: recorder}
	d, err := sql.Open("sqlite", path)
	if err != nil {
		s.record("open", err)
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	d.SetMaxOpenConns(1) // SQLite serializes writes anyway
	s.db = d
	if err := s.migrate(); err != nil {
		_ = d.Close()
		s.record("migrate", err)
		return nil, err
	}
	if err := s.refreshActiveRoomsGauge(); err != nil {
		_ = d.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	err := s.db.Close()
	s.record("close", err)
	return err
}

func (s *Store) record(operation string, err error) {
	if s.recorder == nil {
		return
	}
	if err == nil {
		s.recorder.Inc("pong_sqlite_" + operation + "_success")
	} else {
		s.recorder.Inc("pong_sqlite_" + operation + "_failure")
	}
}

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
	s.record("migrate", err)
	return err
}

func (s *Store) refreshActiveRoomsGauge() error {
	var active, waiting, playing int64
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status = 'waiting' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'playing' THEN 1 ELSE 0 END), 0)
		FROM rooms WHERE status != 'finished'
	`).Scan(&active, &waiting, &playing)
	if err != nil {
		s.record("active_gauge", err)
		return err
	}
	if s.recorder != nil {
		s.recorder.SetGauge("pong_rooms_active", active)
		s.recorder.SetGauge("pong_rooms_waiting", waiting)
		s.recorder.SetGauge("pong_rooms_playing", playing)
	}
	return nil
}

// CreateRoom inserts a new room and returns it.
func (s *Store) CreateRoom(id, name string) (*Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Room{ID: id, Name: name, Status: "waiting", Players: 0, CreatedAt: time.Now().UTC()}
	_, err := s.db.Exec(
		"INSERT INTO rooms (id, name, status, pod_ip, players, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		r.ID, r.Name, r.Status, r.PodIP, r.Players, r.CreatedAt.Format(time.RFC3339Nano),
	)
	s.record("create", err)
	if err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	if s.recorder != nil {
		s.recorder.AddGauge("pong_rooms_active", 1)
		s.recorder.AddGauge("pong_rooms_waiting", 1)
		s.recorder.Inc("pong_rooms_created")
	}
	return r, nil
}

func parseCreatedAt(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	return time.Parse("2006-01-02T15:04:05Z", value)
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
		s.record("get", nil)
		return nil, nil
	}
	if err != nil {
		s.record("get", err)
		return nil, err
	}
	r.CreatedAt, err = parseCreatedAt(created)
	s.record("get", err)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ListRooms returns all rooms that are not finished.
func (s *Store) ListRooms() ([]Room, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT id, name, status, pod_ip, players, created_at FROM rooms WHERE status != 'finished' ORDER BY created_at DESC")
	if err != nil {
		s.record("list", err)
		return nil, err
	}
	defer rows.Close()
	var rooms []Room
	for rows.Next() {
		var r Room
		var created string
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.PodIP, &r.Players, &created); err != nil {
			s.record("list", err)
			return nil, err
		}
		r.CreatedAt, err = parseCreatedAt(created)
		if err != nil {
			s.record("list", err)
			return nil, err
		}
		rooms = append(rooms, r)
	}
	if err := rows.Err(); err != nil {
		s.record("list", err)
		return nil, err
	}
	s.record("list", nil)
	return rooms, nil
}

// UpdateRoomStatus updates a room's status and pod IP.
func (s *Store) UpdateRoomStatus(id, status, podIP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var previous string
	if err := s.db.QueryRow("SELECT status FROM rooms WHERE id = ?", id).Scan(&previous); err != nil && err != sql.ErrNoRows {
		s.record("update_status", err)
		return err
	}
	_, err := s.db.Exec("UPDATE rooms SET status = ?, pod_ip = ? WHERE id = ?", status, podIP, id)
	s.record("update_status", err)
	if err == nil && previous != status && s.recorder != nil {
		if previous == "waiting" {
			s.recorder.AddGauge("pong_rooms_waiting", -1)
		}
		if previous == "playing" {
			s.recorder.AddGauge("pong_rooms_playing", -1)
		}
		if status == "waiting" {
			s.recorder.AddGauge("pong_rooms_waiting", 1)
		}
		if status == "playing" {
			s.recorder.AddGauge("pong_rooms_playing", 1)
		}
	}
	return err
}

// IncrementPlayers atomically increments the player count without allowing a
// room to exceed its two-player capacity.
func (s *Store) IncrementPlayers(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("UPDATE rooms SET players = players + 1 WHERE id = ? AND players < 2", id)
	if err != nil {
		s.record("increment_players", err)
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		s.record("increment_players", err)
		return err
	}
	if updated == 1 {
		s.record("increment_players", nil)
		return nil
	}

	var exists bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id = ?)", id).Scan(&exists); err != nil {
		s.record("increment_players", err)
		return err
	}
	if !exists {
		s.record("increment_players", ErrRoomNotFound)
		return ErrRoomNotFound
	}
	s.record("increment_players", ErrRoomFull)
	return ErrRoomFull
}

// DecrementPlayers releases a reservation when a join response cannot be
// completed. It is intentionally bounded at zero and is safe to retry.
func (s *Store) DecrementPlayers(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec("UPDATE rooms SET players = players - 1 WHERE id = ? AND players > 0", id)
	if err != nil {
		s.record("decrement_players", err)
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		s.record("decrement_players", err)
		return err
	}
	if updated == 1 {
		s.record("decrement_players", nil)
		return nil
	}
	var exists bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id = ?)", id).Scan(&exists); err != nil {
		s.record("decrement_players", err)
		return err
	}
	if !exists {
		s.record("decrement_players", ErrRoomNotFound)
		return ErrRoomNotFound
	}
	s.record("decrement_players", nil)
	return nil
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
		s.record("mark_playing", err)
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		s.record("mark_playing", err)
		return err
	}
	if updated == 1 {
		if s.recorder != nil {
			s.recorder.AddGauge("pong_rooms_waiting", -1)
			s.recorder.AddGauge("pong_rooms_playing", 1)
		}
		s.record("mark_playing", nil)
		return nil
	}

	var status string
	var players int
	err = s.db.QueryRow("SELECT status, players FROM rooms WHERE id = ?", id).Scan(&status, &players)
	if err == sql.ErrNoRows {
		err = ErrRoomNotFound
		s.record("mark_playing", err)
		return err
	}
	if err != nil {
		s.record("mark_playing", err)
		return err
	}
	if status == "playing" {
		s.record("mark_playing", nil)
		return nil
	}
	if status == "finished" {
		err = fmt.Errorf("room is finished")
		s.record("mark_playing", err)
		return err
	}
	err = fmt.Errorf("room is not ready: %d/2 players", players)
	s.record("mark_playing", err)
	return err
}

// DeleteRoom removes a room from the database. It is idempotent.
func (s *Store) DeleteRoom(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var status string
	_ = s.db.QueryRow("SELECT status FROM rooms WHERE id = ?", id).Scan(&status)
	result, err := s.db.Exec("DELETE FROM rooms WHERE id = ?", id)
	if err != nil {
		s.record("delete", err)
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		s.record("delete", err)
		return err
	}
	if deleted == 1 && s.recorder != nil {
		s.recorder.AddGauge("pong_rooms_active", -1)
		if status == "waiting" {
			s.recorder.AddGauge("pong_rooms_waiting", -1)
		}
		if status == "playing" {
			s.recorder.AddGauge("pong_rooms_playing", -1)
		}
		s.recorder.Inc("pong_rooms_cleaned")
	}
	s.record("delete", nil)
	return nil
}
