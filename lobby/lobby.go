// Package lobby implements the lobby server: room CRUD, K8s pod orchestration,
// and routing players to room pods.
package lobby

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudnativepong/db"
)

// RoomHandler is a locally-managed room (used in local/dev mode).
type RoomHandler struct {
	ID   string
	Addr string
	Stop func()
}

// Recorder is the bounded application metrics surface used by the lobby.
type Recorder interface {
	Inc(string)
	AddGauge(string, int64)
	SetGauge(string, int64)
}

type durationRecorder interface {
	ObserveDuration(string, time.Duration)
}

// RoomResource is the privacy-safe subset of orchestration state needed for
// reconciliation tests and production cleanup.
type RoomResource struct {
	RoomID string
	Phase  string
}

// Orchestrator provisions and removes one room's resources. The interface
// keeps lifecycle failure tests dependency-light and cluster-free.
type Orchestrator interface {
	CreateRoom(string) (string, error)
	DeletePod(string) error
	DeleteService(string) error
	ListPods() ([]RoomResource, error)
	ListServices() ([]RoomResource, error)
}

// Server handles HTTP requests for the lobby.
const defaultRoomIdleTimeout = 10 * time.Minute

// Room Pods are created asynchronously. Wait for the Service to publish a
// ready endpoint before returning the room to callers so the lobby's bounded
// WebSocket dial retry budget is reserved for transient routing failures, not
// normal image startup.
const (
	roomEndpointReadyTimeout = 15 * time.Second
	roomEndpointPollInterval = 100 * time.Millisecond
)

const maxJSONBodyBytes int64 = 4 << 10
const maxRoomNameBytes = 80

var roomIDPattern = regexp.MustCompile(`^[0-9a-f]{6}$`)

var (
	ErrInvalidJSON     = errors.New("invalid JSON request")
	ErrBodyTooLarge    = errors.New("request body too large")
	ErrInvalidRoomID   = errors.New("invalid room ID")
	ErrInvalidRoomName = errors.New("name must be 1-80 characters")
)

type Server struct {
	store           dbStore
	mu              sync.RWMutex
	rooms           map[string]*RoomHandler // local mode: in-process rooms
	mode            string                  // "local" or "kubernetes"
	k8sNS           string
	roomImage       string
	roomIdleTimeout time.Duration
	recorder        Recorder
	orchestrator    Orchestrator
}

type dbStore interface {
	CreateRoom(string, string) (*db.Room, error)
	GetRoom(string) (*db.Room, error)
	ListRooms() ([]db.Room, error)
	UpdateRoomStatus(string, string, string) error
	IncrementPlayers(string) error
	DecrementPlayers(string) error
	MarkRoomPlaying(string) error
	DeleteRoom(string) error
}

// NewServer creates a new lobby server with the default abandoned-room timeout.
func NewServer(store *db.Store, mode, k8sNS, roomImage string) *Server {
	return NewServerWithIdleTimeout(store, mode, k8sNS, roomImage, defaultRoomIdleTimeout)
}

// NewServerWithMetrics is the production constructor with the shared bounded
// application metrics recorder.
func NewServerWithMetrics(store *db.Store, mode, k8sNS, roomImage string, recorder Recorder) *Server {
	return NewServerWithDependencies(store, mode, k8sNS, roomImage, defaultRoomIdleTimeout, recorder, nil)
}

// NewServerWithIdleTimeout creates a lobby server with an explicit timeout for
// rooms that never reach the playing state.
func NewServerWithIdleTimeout(store *db.Store, mode, k8sNS, roomImage string, idleTimeout time.Duration) *Server {
	return NewServerWithDependencies(store, mode, k8sNS, roomImage, idleTimeout, nil, nil)
}

// NewServerWithDependencies is the testable constructor. Nil dependencies use
// the production SQLite/Kubernetes implementations.
func NewServerWithDependencies(store dbStore, mode, k8sNS, roomImage string, idleTimeout time.Duration, recorder Recorder, orchestrator Orchestrator) *Server {
	if idleTimeout <= 0 {
		idleTimeout = defaultRoomIdleTimeout
	}
	s := &Server{
		store:           store,
		rooms:           make(map[string]*RoomHandler),
		mode:            mode,
		k8sNS:           k8sNS,
		roomImage:       roomImage,
		roomIdleTimeout: idleTimeout,
		recorder:        recorder,
		orchestrator:    orchestrator,
	}
	if s.orchestrator == nil && mode != "local" {
		s.orchestrator = &kubernetesOrchestrator{server: s}
	}
	return s
}

func (s *Server) metric(name string) {
	if s.recorder != nil {
		s.recorder.Inc(name)
	}
}

func (s *Server) metricResult(operation string, err error) {
	if err == nil {
		s.metric("pong_" + operation + "_success")
	} else {
		s.metric("pong_" + operation + "_failure")
	}
}

func (s *Server) observeDuration(name string, started time.Time) {
	if recorder, ok := s.recorder.(durationRecorder); ok {
		recorder.ObserveDuration(name, time.Since(started))
	}
}

// shortID generates a 6-char hex room ID.
func shortID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateRoom creates a new room (locally or via K8s API).
func (s *Server) CreateRoom(name string) (room *db.Room, err error) {
	started := time.Now()
	defer func() {
		s.metricResult("room_create", err)
		s.observeDuration("pong_room_create_duration_seconds", started)
	}()
	id := shortID()
	room, err = s.store.CreateRoom(id, name)
	if err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}

	switch s.mode {
	case "local":
		// Spawn a room handler in-process (handled by main.go).
		// Just record it — the caller must register the handler.
	case "lobby", "kubernetes":
		if s.orchestrator == nil {
			return nil, errors.New("room orchestrator unavailable")
		}
		podIP, createErr := s.orchestrator.CreateRoom(id)
		s.metricResult("pod_create", createErr)
		if createErr != nil {
			if cleanupErr := s.cleanupResources(id); cleanupErr == nil {
				_ = s.store.DeleteRoom(id)
			}
			return nil, fmt.Errorf("create pod: %w", createErr)
		}
		room.PodIP = podIP
		if updateErr := s.store.UpdateRoomStatus(id, "waiting", podIP); updateErr != nil {
			_ = s.cleanupResources(id)
			_ = s.store.DeleteRoom(id)
			return nil, fmt.Errorf("update room: %w", updateErr)
		}
	}

	return room, nil
}

// RegisterLocalRoom stores a locally-running room handler.
func (s *Server) RegisterLocalRoom(id string, handler *RoomHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rooms[id] = handler
	if err := s.store.UpdateRoomStatus(id, "waiting", "localhost:8080"); err != nil {
		s.metric("pong_room_register_failure")
		return
	}
	s.metric("pong_room_register_success")
}

// GetRoomAddr returns the WebSocket address for a room.
// In local mode, returns the in-process handler address.
// In kubernetes mode, returns the stable ClusterIP Service DNS name.
func (s *Server) GetRoomAddr(id string) (string, error) {
	room, err := s.store.GetRoom(id)
	if err != nil || room == nil {
		return "", fmt.Errorf("room not found")
	}
	if s.mode == "local" {
		s.mu.RLock()
		h, ok := s.rooms[id]
		s.mu.RUnlock()
		if !ok {
			return "", fmt.Errorf("room handler not found")
		}
		return h.Addr, nil
	}
	// Return the stable ClusterIP Service DNS name
	ns := s.k8sNS
	if ns == "" {
		ns = "default"
	}
	return fmt.Sprintf("pong-room-%s.%s.svc.cluster.local:8080", id, ns), nil
}

// ListRooms returns all active rooms.
func (s *Server) ListRooms() ([]db.Room, error) {
	return s.store.ListRooms()
}

// JoinRoom reserves one of the room's two player slots. The room remains in
// waiting status until the room process confirms both WebSocket connections.
func (s *Server) JoinRoom(id string) (err error) {
	started := time.Now()
	defer func() {
		s.metricResult("room_join", err)
		s.observeDuration("pong_room_join_duration_seconds", started)
	}()
	if !ValidRoomID(id) {
		return ErrInvalidRoomID
	}
	room, err := s.store.GetRoom(id)
	if err != nil {
		return err
	}
	if room == nil {
		return db.ErrRoomNotFound
	}
	return s.store.IncrementPlayers(id)
}

// MarkRoomStarted records that both players have connected to the room
// WebSocket. Reservations made through JoinRoom alone never start a room.
func (s *Server) MarkRoomStarted(id string) (err error) {
	started := time.Now()
	defer func() {
		s.metricResult("room_start", err)
		s.observeDuration("pong_room_start_duration_seconds", started)
	}()
	return s.store.MarkRoomPlaying(id)
}

func (s *Server) cleanupResources(roomID string) error {
	if s.mode == "local" {
		return nil
	}
	if s.orchestrator == nil {
		return errors.New("room orchestrator unavailable")
	}
	var firstErr error
	if err := s.orchestrator.DeletePod(roomID); err != nil {
		s.metric("pong_pod_delete_failure")
		firstErr = err
	} else {
		s.metric("pong_pod_delete_success")
	}
	if err := s.orchestrator.DeleteService(roomID); err != nil {
		s.metric("pong_service_delete_failure")
		if firstErr == nil {
			firstErr = err
		}
	} else {
		s.metric("pong_service_delete_success")
	}
	return firstErr
}

// CleanupRoom removes room resources and then deletes its database record. It
// is safe to call repeatedly; a resource deletion failure retains the row so
// restart reconciliation can retry it.
func (s *Server) CleanupRoom(roomID string) (err error) {
	started := time.Now()
	defer func() {
		s.metricResult("room_cleanup", err)
		s.observeDuration("pong_room_cleanup_duration_seconds", started)
	}()
	if err = s.cleanupResources(roomID); err != nil {
		return err
	}
	err = s.store.DeleteRoom(roomID)
	return err
}

func (s *Server) k8sConfig() (token []byte, apiHost, apiPort, ns string, err error) {
	token, err = os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil || len(token) == 0 {
		return nil, "", "", "", fmt.Errorf("kubernetes service account token not found — are we running in a K8s pod?")
	}
	apiHost = os.Getenv("KUBERNETES_SERVICE_HOST")
	apiPort = os.Getenv("KUBERNETES_SERVICE_PORT")
	if apiHost == "" {
		apiHost = "kubernetes.default.svc"
	}
	if apiPort == "" {
		apiPort = "443"
	}
	ns = s.k8sNS
	if ns == "" {
		nsData, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		ns = strings.TrimSpace(string(nsData))
	}
	if ns == "" {
		ns = "default"
	}
	return token, apiHost, apiPort, ns, nil
}

// ReconcileRooms removes terminal or orphaned room resources after an API
// restart. Active rooms remain untouched.
func (s *Server) ReconcileRooms() (err error) {
	started := time.Now()
	defer func() {
		s.metricResult("reconcile", err)
		s.observeDuration("pong_room_reconcile_duration_seconds", started)
	}()
	rooms, err := s.store.ListRooms()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, room := range rooms {
		if room.Status != "waiting" || room.CreatedAt.IsZero() || now.Sub(room.CreatedAt) < s.roomIdleTimeout {
			continue
		}
		if cleanupErr := s.CleanupRoom(room.ID); cleanupErr != nil {
			s.metric("pong_reconcile_cleanup_failure")
		} else {
			s.metric("pong_reconcile_cleanup_success")
		}
	}

	if s.mode == "local" {
		return nil
	}
	if s.orchestrator == nil {
		return errors.New("room orchestrator unavailable")
	}
	pods, err := s.orchestrator.ListPods()
	if err != nil {
		s.metric("pong_pod_list_failure")
		return err
	}
	s.metric("pong_pod_list_success")
	livePods := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		if pod.RoomID == "" {
			continue
		}
		livePods[pod.RoomID] = struct{}{}
		room, getErr := s.store.GetRoom(pod.RoomID)
		if getErr != nil {
			return getErr
		}
		if room == nil || room.Status == "finished" || pod.Phase == "Succeeded" || pod.Phase == "Failed" || pod.Phase == "Unknown" {
			if cleanupErr := s.CleanupRoom(pod.RoomID); cleanupErr != nil {
				s.metric("pong_reconcile_cleanup_failure")
			} else {
				s.metric("pong_reconcile_cleanup_success")
			}
		}
	}

	services, err := s.orchestrator.ListServices()
	if err != nil {
		s.metric("pong_service_list_failure")
		return err
	}
	s.metric("pong_service_list_success")
	liveServices := make(map[string]struct{}, len(services))
	for _, service := range services {
		if service.RoomID == "" {
			continue
		}
		liveServices[service.RoomID] = struct{}{}
		if _, ok := livePods[service.RoomID]; ok {
			continue
		}
		if cleanupErr := s.CleanupRoom(service.RoomID); cleanupErr != nil {
			s.metric("pong_reconcile_cleanup_failure")
		} else {
			s.metric("pong_reconcile_cleanup_success")
		}
	}

	// A room that reached playing must have both resources. If the Pod or its
	// Service disappeared without delivering the finish callback, remove the
	// persisted row as well; otherwise every list request retains a dead room
	// and each restart repeats the same orphan indefinitely.
	for _, room := range rooms {
		if room.Status != "playing" {
			continue
		}
		_, podExists := livePods[room.ID]
		_, serviceExists := liveServices[room.ID]
		if podExists && serviceExists {
			continue
		}
		if cleanupErr := s.CleanupRoom(room.ID); cleanupErr != nil {
			s.metric("pong_reconcile_cleanup_failure")
		} else {
			s.metric("pong_reconcile_cleanup_success")
		}
	}
	return nil
}

// ---- HTTP Handlers ----

// DecodeJSONBody parses one bounded JSON object and rejects unknown fields or
// trailing data. Public endpoints use this helper for a uniform body contract.
func DecodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if contentType != "application/json" {
		return ErrInvalidJSON
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "request body too large") || errors.Is(err, http.ErrBodyReadAfterClose) {
			return ErrBodyTooLarge
		}
		return ErrInvalidJSON
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil && strings.Contains(err.Error(), "request body too large") {
			return ErrBodyTooLarge
		}
		return ErrInvalidJSON
	}
	return nil
}

// ValidRoomID reports whether id is one of the server-generated room IDs.
func ValidRoomID(id string) bool { return roomIDPattern.MatchString(id) }

// ValidRoomName trims and validates a public display name.
func ValidRoomName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]byte(name)) > maxRoomNameBytes {
		return "", ErrInvalidRoomName
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", ErrInvalidRoomName
		}
	}
	return name, nil
}

// RequestErrorStatus maps a public request error to its HTTP status.
func RequestErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrInvalidJSON), errors.Is(err, ErrInvalidRoomID), errors.Is(err, ErrInvalidRoomName):
		return http.StatusBadRequest
	case errors.Is(err, db.ErrRoomNotFound):
		return http.StatusNotFound
	case errors.Is(err, db.ErrRoomFull):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := RequestErrorStatus(err)
	if status >= 500 {
		http.Error(w, "internal server error", status)
		return
	}
	http.Error(w, err.Error(), status)
}

// HandleRoomStarted marks a room as playing after both room WebSocket
// connections have been accepted.
func (s *Server) HandleRoomStarted(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.MarkRoomStarted(roomID); err != nil {
		s.metric("pong_room_start_callback_failure")
		http.Error(w, "room start rejected", http.StatusConflict)
		return
	}
	s.metric("pong_room_start_callback_success")
	w.WriteHeader(http.StatusNoContent)
}

// HandleRoomFinished removes a completed room's Pod, Service, and record.
func (s *Server) HandleRoomFinished(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.CleanupRoom(roomID); err != nil {
		s.metric("pong_room_finish_failure")
		http.Error(w, "room cleanup unavailable", http.StatusInternalServerError)
		return
	}
	s.metric("pong_room_finish_success")
	w.WriteHeader(http.StatusNoContent)
}

// HandleListRooms returns JSON list of active rooms.
func (s *Server) HandleListRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rooms, err := s.ListRooms()
	if err != nil {
		s.metric("pong_room_list_failure")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.metric("pong_room_list_success")
	if rooms == nil {
		rooms = []db.Room{}
	}
	writeJSON(w, rooms)
}

// HandleCreateRoom creates a new room and returns its info.
func (s *Server) HandleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := DecodeJSONBody(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	name, err := ValidRoomName(req.Name)
	if err != nil {
		writeError(w, err)
		return
	}

	room, err := s.CreateRoom(name)
	if err != nil {
		s.metric("pong_room_create_http_failure")
		http.Error(w, "room service unavailable", http.StatusServiceUnavailable)
		return
	}
	// Creating a room is the creator's reservation. The browser opens the
	// creator's WebSocket after this response, so reserving here keeps the
	// database capacity model aligned with the public create workflow.
	if err := s.JoinRoom(room.ID); err != nil {
		if cleanupErr := s.CleanupRoom(room.ID); cleanupErr != nil {
			log.Printf("event=room_create_cleanup_failed")
		}
		s.metric("pong_room_create_http_failure")
		writeError(w, err)
		return
	}
	room.Players = 1
	s.metric("pong_room_create_http_success")
	writeJSON(w, room)
}

// HandleJoinRoom validates room capacity and returns connection info.
func (s *Server) HandleJoinRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RoomID string `json:"room_id"`
	}
	if err := DecodeJSONBody(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if !ValidRoomID(req.RoomID) {
		writeError(w, ErrInvalidRoomID)
		return
	}

	if err := s.JoinRoom(req.RoomID); err != nil {
		s.metric("pong_room_join_http_failure")
		writeError(w, err)
		return
	}

	addr, err := s.GetRoomAddr(req.RoomID)
	if err != nil {
		_ = s.store.DecrementPlayers(req.RoomID)
		s.metric("pong_room_join_http_failure")
		writeError(w, err)
		return
	}

	s.metric("pong_room_join_http_success")
	writeJSON(w, map[string]string{
		"room_id": req.RoomID,
		"ws_addr": addr,
		"ws_path": "/rooms/" + req.RoomID + "/ws",
		"mode":    s.mode,
	})
}

// ---- Kubernetes integration ----

type kubernetesOrchestrator struct{ server *Server }

func (o *kubernetesOrchestrator) CreateRoom(roomID string) (string, error) {
	return o.server.createK8sPod(roomID)
}

func (o *kubernetesOrchestrator) DeletePod(roomID string) error {
	return o.server.deleteK8sResource("pods/pong-room-" + roomID)
}

func (o *kubernetesOrchestrator) DeleteService(roomID string) error {
	return o.server.deleteK8sResource("services/pong-room-" + roomID)
}

func (o *kubernetesOrchestrator) ListPods() ([]RoomResource, error) {
	return o.server.listK8sResources("pods", true)
}

func (o *kubernetesOrchestrator) ListServices() ([]RoomResource, error) {
	return o.server.listK8sResources("services", false)
}

func (s *Server) deleteK8sResource(resource string) error {
	token, apiHost, apiPort, ns, err := s.k8sConfig()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/%s", apiHost, apiPort, ns, resource)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := k8sClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("kubernetes delete returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) listK8sResources(resource string, includePhase bool) ([]RoomResource, error) {
	token, apiHost, apiPort, ns, err := s.k8sConfig()
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/%s?labelSelector=role%%3Droom", apiHost, apiPort, ns, resource)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := k8sClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kubernetes list returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	resources := make([]RoomResource, 0, len(result.Items))
	for _, item := range result.Items {
		roomID := item.Metadata.Labels["room-id"]
		if !ValidRoomID(roomID) {
			continue
		}
		phase := ""
		if includePhase {
			phase = item.Status.Phase
		}
		resources = append(resources, RoomResource{RoomID: roomID, Phase: phase})
	}
	return resources, nil
}

// createK8sPod creates a new pod for a game room using the K8s REST API.
func (s *Server) createK8sPod(roomID string) (string, error) {
	// Read the service account token and CA from the pod's filesystem.
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil || len(token) == 0 {
		return "", fmt.Errorf("kubernetes service account token not found — are we running in a K8s pod?")
	}
	apiHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	apiPort := os.Getenv("KUBERNETES_SERVICE_PORT")
	if apiHost == "" {
		apiHost = "kubernetes.default.svc"
	}
	if apiPort == "" {
		apiPort = "443"
	}

	ns := s.k8sNS
	if ns == "" {
		nsData, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		ns = strings.TrimSpace(string(nsData))
	}
	if ns == "" {
		ns = "default"
	}

	image := s.roomImage
	if image == "" {
		image = "cloudnativepong-room:latest"
	}

	pod := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name": "pong-room-" + roomID,
			"labels": map[string]string{
				"app":     "cloudnativepong",
				"room-id": roomID,
				"role":    "room",
			},
		},
		"spec": map[string]interface{}{
			"automountServiceAccountToken": false,
			"securityContext": map[string]interface{}{
				"runAsNonRoot": true,
				"runAsUser":    65532,
				"runAsGroup":   65532,
				"seccompProfile": map[string]interface{}{
					"type": "RuntimeDefault",
				},
			},
			"containers": []map[string]interface{}{
				{
					"name":            "pong-room",
					"image":           image,
					"imagePullPolicy": "IfNotPresent",
					"args":            []string{"--mode=room", "--room-id=" + roomID, "--lobby-addr=" + s.lobbyAddr()},
					"env": []map[string]interface{}{
						{"name": "PONG_ALLOWED_ORIGINS", "value": s.allowedOrigins()},
					},
					"ports": []map[string]interface{}{
						{"containerPort": 8080},
					},
					"readinessProbe": map[string]interface{}{
						"httpGet":             map[string]interface{}{"path": "/health", "port": 8080},
						"initialDelaySeconds": 1,
						"periodSeconds":       5,
					},
					"livenessProbe": map[string]interface{}{
						"httpGet":             map[string]interface{}{"path": "/health", "port": 8080},
						"initialDelaySeconds": 5,
						"periodSeconds":       10,
					},
					"securityContext": map[string]interface{}{
						"allowPrivilegeEscalation": false,
						"capabilities": map[string]interface{}{
							"drop": []string{"ALL"},
						},
						"readOnlyRootFilesystem": true,
						"runAsNonRoot":           true,
					},
					"resources": map[string]interface{}{
						"requests": map[string]string{
							"cpu":    "50m",
							"memory": "32Mi",
						},
						"limits": map[string]string{
							"cpu":    "200m",
							"memory": "64Mi",
						},
					},
				},
			},
			"restartPolicy":                 "Never",
			"activeDeadlineSeconds":         7200,
			"terminationGracePeriodSeconds": 5,
		},
	}

	// Create the ClusterIP Service before the pod. Routing uses this stable
	// Service DNS name, so a Service failure makes the room unusable and must
	// fail provisioning rather than leaving an orphaned database reservation.
	if err := s.createK8sService(roomID, apiHost, apiPort, ns, token); err != nil {
		s.metric("pong_service_create_failure")
		return "", fmt.Errorf("create service: %w", err)
	}
	s.metric("pong_service_create_success")

	body, _ := json.Marshal(pod)
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/pods", apiHost, apiPort, ns)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}

	client := k8sClient()
	// In-cluster we need to trust the CA
	// For simplicity, skip TLS verify when CA cert is used (prod should verify)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("k8s api call: %w", err)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return "", errors.New("kubernetes pod response unreadable")
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("kubernetes pod create returned HTTP %d", resp.StatusCode)
	}

	// The Pod may still be Pending when the API returns. Routing uses the
	// per-room Service DNS name rather than this transient Pod IP, so do not
	// block room creation on scheduler/image-pull latency. Preserve an IP when
	// Kubernetes assigned one immediately for diagnostics only.
	var result struct {
		Status struct {
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", errors.New("invalid kubernetes pod response")
	}
	if err := s.waitForRoomEndpoint(roomID, apiHost, apiPort, ns, token); err != nil {
		s.metric("pong_room_ready_wait_failure")
		return "", err
	}
	s.metric("pong_room_ready_wait_success")
	return result.Status.PodIP, nil
}

// waitForRoomEndpoint waits for the room Service to have at least one ready
// endpoint. A successful Pod POST only means that Kubernetes accepted the
// object; it does not mean that DNS/service routing can reach the container.
// The bounded wait keeps startup failure explicit without multiplying public
// retries or leaving an unusable room reservation behind.
func (s *Server) waitForRoomEndpoint(roomID, apiHost, apiPort, ns string, token []byte) error {
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/endpoints/pong-room-%s", apiHost, apiPort, ns, roomID)
	deadline := time.Now().Add(roomEndpointReadyTimeout)
	var lastErr error
	for {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("create room endpoint request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
		resp, requestErr := k8sClient().Do(req)
		if requestErr == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			switch {
			case readErr != nil:
				lastErr = errors.New("kubernetes endpoint response unreadable")
			case resp.StatusCode == http.StatusOK:
				if roomEndpointHasReadyAddress(body) {
					return nil
				}
				lastErr = errors.New("room endpoint has no ready address")
			case resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500:
				lastErr = fmt.Errorf("kubernetes endpoint returned HTTP %d", resp.StatusCode)
			default:
				return fmt.Errorf("kubernetes endpoint returned HTTP %d", resp.StatusCode)
			}
		} else {
			lastErr = requestErr
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr == nil {
				lastErr = errors.New("room endpoint did not become ready")
			}
			return fmt.Errorf("room endpoint did not become ready: %w", lastErr)
		}
		wait := roomEndpointPollInterval
		if remaining < wait {
			wait = remaining
		}
		time.Sleep(wait)
	}
}

func roomEndpointHasReadyAddress(body []byte) bool {
	var endpoint struct {
		Subsets []struct {
			Addresses []struct {
				IP string `json:"ip"`
			} `json:"addresses"`
		} `json:"subsets"`
	}
	if json.Unmarshal(body, &endpoint) != nil {
		return false
	}
	for _, subset := range endpoint.Subsets {
		for _, address := range subset.Addresses {
			if strings.TrimSpace(address.IP) != "" {
				return true
			}
		}
	}
	return false
}

// createK8sService creates a ClusterIP Service for a room pod so it has a
// stable DNS name: pong-room-{id}.{namespace}.svc.cluster.local
func (s *Server) createK8sService(roomID, apiHost, apiPort, ns string, token []byte) error {
	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name": "pong-room-" + roomID,
			"labels": map[string]string{
				"app":     "cloudnativepong",
				"room-id": roomID,
				"role":    "room",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]string{
				"app":     "cloudnativepong",
				"room-id": roomID,
				"role":    "room",
			},
			"ports": []map[string]interface{}{
				{
					"protocol":   "TCP",
					"port":       8080,
					"targetPort": 8080,
				},
			},
			"type": "ClusterIP",
		},
	}

	body, _ := json.Marshal(svc)
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/services", apiHost, apiPort, ns)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}

	client := k8sClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("k8s service api call: %w", err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return errors.New("kubernetes service response unreadable")
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("kubernetes service create returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// allowedOrigins returns the exact origin policy that room Pods must use for
// their browser-facing WebSocket handler. Keeping this on the generated Pod
// avoids a lobby/room policy split in local and Kubernetes test environments.
func (s *Server) allowedOrigins() string {
	if configured := strings.TrimSpace(os.Getenv("PONG_ALLOWED_ORIGINS")); configured != "" {
		return configured
	}
	if s.mode == "local" {
		return "http://localhost:8080,http://127.0.0.1:8080,http://[::1]:8080"
	}
	return "https://pong.belacca.com"
}

// lobbyAddr returns the lobby's address for room pods to report back to.
// In K8s mode, the lobby Service is reachable at pong-lobby.{namespace}.svc.cluster.local:8080.
// In local mode, falls back to the node's non-loopback IPv4 address.
func (s *Server) lobbyAddr() string {
	host := os.Getenv("LOBBY_ADDR")
	if host != "" {
		return host
	}
	// In K8s, the lobby Service DNS name is deterministic
	if s.mode == "lobby" || s.mode == "kubernetes" {
		ns := s.k8sNS
		if ns == "" {
			ns = "default"
		}
		return fmt.Sprintf("pong-api.%s.svc.cluster.local:8080", ns)
	}
	// Fallback for local mode: use the node's IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String() + ":8080"
		}
	}
	return "localhost:8080"
}

var (
	k8sClientOnce sync.Once
	k8sHTTPClient *http.Client
)

// k8sClient creates the shared HTTP client configured with the K8s API CA
// cert. Reusing its transport is important: reconciliation runs continuously,
// and creating a new transport for every list/create/delete operation retains
// idle connections and transport state until garbage collection.
func k8sClient() *http.Client {
	k8sClientOnce.Do(func() {
		const caCertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
		tlsCfg := &tls.Config{}
		caData, err := os.ReadFile(caCertPath)
		if err == nil {
			pool, _ := x509.SystemCertPool()
			if pool == nil {
				pool = x509.NewCertPool()
			}
			pool.AppendCertsFromPEM(caData)
			tlsCfg.RootCAs = pool
		}
		k8sHTTPClient = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	})
	return k8sHTTPClient
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
