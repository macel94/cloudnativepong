# Cloud Native Pong — Handoff Document

## Prerequisites (installed on this machine)

| Tool | Version | Notes |
|------|---------|-------|
| Go | 1.25.0 | Must be ≥1.25.0 for `modernc.org/sqlite` |
| Node.js | 24.18.1 | For Playwright E2E tests |
| npm | 11.16.0 | |
| Podman | 5.4.2 | Container builds (no Docker) |
| k3d | 5.9.0 | Local K8s cluster |
| kubectl | 1.31.0 | |
| Playwright | 1.62.0 | Chromium only |

## Architecture

```
Gateway (Caddy :80, NodePort 30080)
├── /              → Static (nginx:alpine, ClusterIP :80)
├── /api/*         → API (Go, ClusterIP :8080, SQLite + PVC)
├── /rooms/{id}/ws → API → Room Pod (dynamic, ClusterIP :8080)
└── /style.css etc → Static (nginx)
```

### 4 Docker images

| Dockerfile | Image | Base | Size |
|-----------|-------|------|------|
| Dockerfile.api | `cloudnativepong-api` | golang:1.25-alpine → scratch | ~12 MB |
| Dockerfile.room | `cloudnativepong-room` | golang:1.25-alpine → scratch | ~12 MB |
| Dockerfile.static | `cloudnativepong-static` | nginx:alpine | ~64 MB |
| Dockerfile.gateway | `cloudnativepong-gateway` | caddy:alpine | ~63 MB |

**Important**: Podman tags images with `localhost/` prefix. In k3d, images are referenced as `localhost/cloudnativepong-api:latest` etc. The k8s manifests must use this prefix. In CI (Docker), drop the `localhost/` prefix.

## What Works (k3d integration)

- ✅ **K8s manifests** (`k8s/all.yaml`): 4 Deployments, 5 Services, PVC, RBAC, ConfigMap
- ✅ **All 4 images build** with podman and import into k3d
- ✅ **Gateway** (Caddy) routes correctly: `/ → static`, `/api/* → API`, `/rooms/* → API`
- ✅ **Static** (nginx) serves HTML/CSS/JS
- ✅ **API** (Go lobby mode) creates rooms, manages DB
- ✅ **Room pod creation**: API dynamically creates K8s pods + ClusterIP Services
- ✅ **DNS**: `pong-room-{id}.pong.svc.cluster.local` resolves to room pod
- ✅ **11/12 E2E tests pass** against k3d deployment
- ✅ **Local mode** (single binary): all 12/12 tests pass
- ✅ **TLS for K8s API**: `k8sClient()` reads CA cert from SA token volume
- ✅ **Retry logic**: Lobby retries TCP connection to room pod (10 attempts, 500ms apart)
- ✅ **Room pod image**: `localhost/cloudnativepong-room:latest` with `imagePullPolicy: Never`

## What Doesn't Work (1 remaining issue)

### ❌ Two-player game WebSocket test (test #7)

**Symptom**: Player 1 connects via WebSocket, status shows "Waiting for opponent...", never receives "joined" message from room pod.

**Root cause analysis**: The WebSocket proxy in `main.go:proxyRoomWS()` uses a raw TCP hijack approach:
1. Lobby hijacks the HTTP connection from Caddy (after Caddy forwards the WS upgrade)
2. Lobby writes 101 Switching Protocols response to Caddy/browser
3. Lobby opens raw TCP to room pod, sends WS upgrade request
4. Lobby reads 101 response from room pod **(byte-by-byte until `\r\n\r\n`)**
5. Lobby relays raw bytes bidirectionally: `io.Copy(targetConn, clientConn)` + `io.Copy(clientConn, targetConn)`

**What was tried**:
1. Original gorilla/websocket upgrade both sides → Caddy reverse_proxy + gorilla Upgrade race condition → "abnormal closure: unexpected EOF" in room pod
2. Raw TCP hijack (current) → connection stays open but room pod's "joined" message never reaches browser
3. Byte-by-byte reading of room pod's 101 response (to avoid consuming the first WS frame) → still not working
4. Connection retry on room pod → fixed "connection refused" race condition

**Likely cause**: The `io.Copy(clientConn, targetConn)` goroutine (room→browser) starts AFTER the 101 response is read from the room pod. But the room pod sends the "joined" WebSocket frame BEFORE the `io.Copy` goroutine starts. The frame bytes might arrive during the 101 read phase and be lost, OR there's a timing issue where the frame arrives between the end of 101 read and the start of io.Copy.

**Debugging hints**:
- Room pod logs show "Player 1 joined room XXXX" → room pod is working correctly
- Room pod logs show later "Player 1 disconnected: websocket: close 1001 (going away)" → browser disconnects (no data received)
- Browser test shows "Waiting for opponent..." → WebSocket is OPEN but no messages arrive

**Potential fixes to try**:
1. Use a gorilla/websocket connection on the room-pod side (not raw TCP) to properly parse the 101 response and WS frames
2. Start `io.Copy` goroutines BEFORE reading room pod's 101 response, buffer the 101 response separately
3. Replace the lobby proxy entirely: expose room pod WebSocket via a separate NodePort, have browser connect directly
4. Use WebSocket-aware proxying on both sides (original approach but fix the Caddy race)

## Environment

- **Repo**: `https://github.com/macel94/cloudnativepong` (private)
- **Current branch**: `main`
- **Commit**: Latest includes all the k3d fixes

## Key Files Changed

| File | Purpose |
|------|---------|
| `main.go` | Entry point, routing, `proxyRoomWS` (hijack-based WS proxy) |
| `lobby/lobby.go` | Room CRUD, K8s API integration, pod/service creation |
| `gateway/Caddyfile` | Caddy routing: `{env.API_ADDR}`, `{env.STATIC_ADDR}` |
| `static/nginx.conf` | nginx config with CORS, gzip, security headers |
| `k8s/all.yaml` | All K8s resources (374 lines) |
| `Dockerfile.*` | 4 separate Dockerfiles |
| `tests/e2e.spec.ts` | 12 Playwright tests |
| `playwright.config.ts` | webServer config, baseURL, cwd |
| `.github/workflows/test.yml` | 3 CI jobs: local E2E, build images, k3d E2E |

## K8s Deployment Quick Reference

```bash
# Build all images
podman build -t localhost/cloudnativepong-api:latest -f Dockerfile.api .
podman build -t localhost/cloudnativepong-room:latest -f Dockerfile.room .
podman build -t localhost/cloudnativepong-static:latest -f Dockerfile.static .
podman build -t localhost/cloudnativepong-gateway:latest -f Dockerfile.gateway .

# Create cluster and import images
k3d cluster create pong --agents 2 --port 8080:80@loadbalancer
podman save localhost/cloudnativepong-api:latest -o /tmp/api.tar && k3d image import /tmp/api.tar -c pong
podman save localhost/cloudnativepong-room:latest -o /tmp/room.tar && k3d image import /tmp/room.tar -c pong
podman save localhost/cloudnativepong-static:latest -o /tmp/static.tar && k3d image import /tmp/static.tar -c pong
podman save localhost/cloudnativepong-gateway:latest -o /tmp/gateway.tar && k3d image import /tmp/gateway.tar -c pong

# Deploy
kubectl apply -f k8s/all.yaml
kubectl -n pong wait --for=condition=ready pod -l app=cloudnativepong --timeout=60s

# Access (k3d LB on :8080 proxies to NodePort :30080)
# OR use port-forward:
kubectl -n pong port-forward svc/pong-gateway 8080:80
```

## Test Commands

```bash
# Local mode (fast, no K8s)
npx playwright test --reporter=list

# Against k3d
kubectl -n pong port-forward svc/pong-gateway 8080:80 &
npx playwright test --reporter=list
```

## Running k3d Cluster

```bash
# Check status
kubectl -n pong get pods,svc

# View logs
kubectl -n pong logs -l component=api --tail=20
kubectl -n pong logs -l role=room --tail=20

# Cleanup room pods (they accumulate)
kubectl -n pong delete pods -l role=room --force
kubectl -n pong delete svc -l role=room

# Wipe everything
k3d cluster delete pong
```

## Notes for the Next Developer

1. **podman vs docker**: All local commands use `podman`. The CI workflow uses `docker` (GitHub Actions). Images are tagged with `localhost/` prefix by podman; remove it for docker.

2. **Room pod cleanup**: Room pods are never deleted automatically. The lobby should delete them when the game finishes. This is a TODO.

3. **The WS proxy is the hard part**: The Caddy → Lobby (hijack) → Room Pod (raw TCP) approach is tricky. Consider:
   - Using Caddy's `reverse_proxy` directly to room pods (requires dynamic Caddy config)
   - Or having the lobby NOT hijack but instead use a proper WebSocket library on BOTH sides
   - Or bypass the lobby entirely for WebSocket by having room pods on a separate NodePort

4. **Test data isolation**: Tests create room pods that persist. Add cleanup in `global-setup.ts` or `afterAll`.

5. **Gorilla WebSocket**: v1.5.3 is already in go.mod. The `upgrader` is used in local mode and room mode but NOT in the lobby proxy (raw TCP hijack instead).