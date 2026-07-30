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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
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

// Server handles HTTP requests for the lobby.
type Server struct {
	store     *db.Store
	mu        sync.RWMutex
	rooms     map[string]*RoomHandler // local mode: in-process rooms
	mode      string                  // "local" or "kubernetes"
	k8sNS     string
	roomImage string
}

// NewServer creates a new lobby server.
func NewServer(store *db.Store, mode, k8sNS, roomImage string) *Server {
	return &Server{
		store:     store,
		rooms:     make(map[string]*RoomHandler),
		mode:      mode,
		k8sNS:     k8sNS,
		roomImage: roomImage,
	}
}

// shortID generates a 6-char hex room ID.
func shortID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateRoom creates a new room (locally or via K8s API).
func (s *Server) CreateRoom(name string) (*db.Room, error) {
	id := shortID()
	room, err := s.store.CreateRoom(id, name)
	if err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}

	switch s.mode {
	case "local":
		// Spawn a room handler in-process (handled by main.go)
		// Just record it — the caller must register the handler.
	case "lobby", "kubernetes":
		// Create a K8s pod for this room
		podIP, err := s.createK8sPod(id)
		if err != nil {
			s.store.DeleteRoom(id)
			return nil, fmt.Errorf("create pod: %w", err)
		}
		room.PodIP = podIP
		s.store.UpdateRoomStatus(id, "waiting", podIP)
	}

	return room, nil
}

// RegisterLocalRoom stores a locally-running room handler.
func (s *Server) RegisterLocalRoom(id string, handler *RoomHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rooms[id] = handler
	s.store.UpdateRoomStatus(id, "waiting", "localhost:8080")
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

// JoinRoom increments the player count. Returns error if room is full.
func (s *Server) JoinRoom(id string) error {
	room, err := s.store.GetRoom(id)
	if err != nil || room == nil {
		return fmt.Errorf("room not found")
	}
	if room.Players >= 2 {
		return fmt.Errorf("room is full")
	}
	return s.store.IncrementPlayers(id)
}

// CleanupRoom removes the Kubernetes Pod and Service for a room and deletes
// its database record. It is safe to call more than once.
func (s *Server) CleanupRoom(roomID string) error {
	if s.mode == "local" {
		return s.store.DeleteRoom(roomID)
	}

	token, apiHost, apiPort, ns, err := s.k8sConfig()
	if err != nil {
		return err
	}
	for _, resource := range []string{"pods/pong-room-" + roomID, "services/pong-room-" + roomID} {
		url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/%s", apiHost, apiPort, ns, resource)
		req, err := http.NewRequest(http.MethodDelete, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
		resp, err := k8sClient().Do(req)
		if err != nil {
			return fmt.Errorf("delete %s: %w", resource, err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("delete %s returned %d", resource, resp.StatusCode)
		}
	}
	return s.store.DeleteRoom(roomID)
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
func (s *Server) ReconcileRooms() error {
	if s.mode == "local" {
		return nil
	}
	token, apiHost, apiPort, ns, err := s.k8sConfig()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/pods?labelSelector=role%%3Droom", apiHost, apiPort, ns)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := k8sClient().Do(req)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("list room pods returned %d: %s", resp.StatusCode, body)
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
		return err
	}
	for _, item := range result.Items {
		roomID := item.Metadata.Labels["room-id"]
		if roomID == "" {
			continue
		}
		room, err := s.store.GetRoom(roomID)
		if err != nil {
			return err
		}
		if room == nil || room.Status == "finished" || item.Status.Phase == "Succeeded" || item.Status.Phase == "Failed" {
			if err := s.CleanupRoom(roomID); err != nil {
				log.Printf("reconcile: cleanup room %s: %v", roomID, err)
			}
		}
	}

	// A crash can leave a Service after its Pod has already disappeared. Clean
	// those orphan Services as well; active room Services are retained.
	url = fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/services?labelSelector=role%%3Droom", apiHost, apiPort, ns)
	req, err = http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err = k8sClient().Do(req)
	if err != nil {
		return err
	}
	body, readErr = io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("list room services returned %d: %s", resp.StatusCode, body)
	}
	var services struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &services); err != nil {
		return err
	}
	for _, item := range services.Items {
		roomID := item.Metadata.Labels["room-id"]
		if roomID == "" {
			continue
		}
		room, err := s.store.GetRoom(roomID)
		if err != nil {
			return err
		}
		if room == nil || room.Status == "finished" {
			if err := s.CleanupRoom(roomID); err != nil {
				log.Printf("reconcile: cleanup orphan service for room %s: %v", roomID, err)
			}
		}
	}
	return nil
}

// ---- HTTP Handlers ----

// HandleRoomFinished removes a completed room's Pod, Service, and record.
func (s *Server) HandleRoomFinished(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.CleanupRoom(roomID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleListRooms returns JSON list of active rooms.
func (s *Server) HandleListRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.ListRooms()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rooms == nil {
		rooms = []db.Room{}
	}
	writeJSON(w, rooms)
}

// HandleCreateRoom creates a new room and returns its info.
func (s *Server) HandleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	room, err := s.CreateRoom(req.Name)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, room)
}

// HandleJoinRoom validates room capacity and returns connection info.
func (s *Server) HandleJoinRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		RoomID string `json:"room_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if err := s.JoinRoom(req.RoomID); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}

	addr, err := s.GetRoomAddr(req.RoomID)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]string{
		"room_id": req.RoomID,
		"ws_addr": addr,
		"ws_path": "/rooms/" + req.RoomID + "/ws",
		"mode":    s.mode,
	})
}

// ---- Kubernetes integration ----

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
			"containers": []map[string]interface{}{
				{
					"name":            "pong-room",
					"image":           image,
					"imagePullPolicy": "IfNotPresent",
					"args":            []string{"--mode=room", "--room-id=" + roomID, "--lobby-addr=" + s.lobbyAddr()},
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
					"activeDeadlineSeconds": 7200,
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
			"terminationGracePeriodSeconds": 5,
		},
	}

	// Create the ClusterIP Service for the room (before the pod, so DNS is ready)
	if err := s.createK8sService(roomID, apiHost, apiPort, ns, token); err != nil {
		log.Printf("Warning: failed to create service for room %s: %v", roomID, err)
		// Non-fatal: the pod IP can still be used directly
	}

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

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("k8s api returned %d: %s", resp.StatusCode, string(respBody))
	}
	log.Printf("Created pod for room %s: %s", roomID, string(respBody))

	// Parse the pod IP from the response
	var result map[string]interface{}
	json.Unmarshal(respBody, &result)
	status := result["status"].(map[string]interface{})
	podIP := ""
	if ip, ok := status["podIP"]; ok {
		podIP = ip.(string)
	}

	// Wait briefly for pod IP to be assigned
	if podIP == "" {
		podIP = s.waitForPodIP(roomID, string(token), apiHost, apiPort, ns)
		if podIP == "" {
			return "", fmt.Errorf("timed out waiting for pod IP for room %s", roomID)
		}
	}

	return podIP, nil
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

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("k8s service api returned %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("Created ClusterIP Service for room %s (pong-room-%s.%s.svc.cluster.local)", roomID, roomID, ns)
	return nil
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

func (s *Server) waitForPodIP(roomID, token, apiHost, apiPort, ns string) string {
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/pods/pong-room-%s", apiHost, apiPort, ns, roomID)
	for i := 0; i < 15; i++ {
		time.Sleep(1 * time.Second)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		client := k8sClient()
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var result map[string]interface{}
		json.Unmarshal(body, &result)
		if status, ok := result["status"].(map[string]interface{}); ok {
			if ip, ok := status["podIP"].(string); ok && ip != "" {
				return ip
			}
		}
	}
	return ""
}

// k8sClient creates an HTTP client configured with the K8s API CA cert.
func k8sClient() *http.Client {
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
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
