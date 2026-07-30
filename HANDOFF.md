# Cloud Native Pong — Handoff Document

## Status

This document describes the gateway/proxy changes on the `main` branch. The implementation and the full k3d Playwright path are now cleanly verified: 12/12 tests pass when host traffic is mapped to the application's gateway NodePort. The earlier intermittent 11/12 result was caused by an invalid or unstable test access path, not a reproducible room/proxy failure.

**Execution model for this change:** `openai/gpt-5.6-luna` via OpenRouter. No other model was used for the implementation or verification work.

## Prerequisites

| Tool | Version | Notes |
|------|---------|-------|
| Go | 1.25.0 | Must be ≥1.25.0 for `modernc.org/sqlite` |
| Node.js | 24.18.1 | For Playwright E2E tests |
| npm | 11.16.0 | |
| Podman | 5.4.2 | Container builds; use Podman rather than Docker locally |
| k3d | 5.9.0 | Local Kubernetes |
| kubectl | 1.31.0 | |
| Playwright | 1.62.0 | Chromium E2E coverage |

## Current Architecture

```
Gateway (NGINX :80, NodePort 30080)
├── /              → Static (nginx:alpine, ClusterIP :80)
├── /api/*         → API/lobby (Go, ClusterIP :8080)
├── /rooms/{id}/ws → API/lobby → room pod (dynamic, ClusterIP :8080)
└── /style.css etc → Static (nginx)
```

The four images are:

| Dockerfile | Image | Base | Role |
|-----------|-------|------|------|
| `Dockerfile.api` | `cloudnativepong-api` | `golang:1.25-alpine` → `scratch` | Lobby/API and room proxy |
| `Dockerfile.room` | `cloudnativepong-room` | `golang:1.25-alpine` → `scratch` | One game room per pod |
| `Dockerfile.static` | `cloudnativepong-static` | `nginx:alpine` | Frontend assets |
| `Dockerfile.gateway` | `cloudnativepong-gateway` | `nginx:alpine` | Browser-facing HTTP/WebSocket gateway |

## Changes Included

### Gateway replacement

The browser-facing Caddy image/configuration was replaced with NGINX:

- `Dockerfile.gateway` now builds from `nginx:alpine`.
- `gateway/nginx.conf` routes `/api/` and `/rooms/` to `pong-api`, and all other paths to `pong-static`.
- The `/rooms/` location sets HTTP/1.1 upgrade headers, disables request and response buffering, and uses one-hour read/send timeouts.
- `/health` is served locally by NGINX for the Kubernetes readiness probe.
- The old `gateway/Caddyfile` and gateway-only Caddy environment variables are no longer used.

The change was made after investigating the Caddy reverse-proxy handoff behavior, including Caddy issue [#6273](https://github.com/caddyserver/caddy/issues/6273), “Missing byte in first websocket message.” The issue describes how buffered bytes around a `101` response can be lost if a tunnel copier reads the raw connection instead of preserving the buffered reader state. NGINX removes the Caddy-specific outer handoff behavior, but it does not by itself eliminate the custom lobby proxy's remaining timing sensitivity.

### Room-side WebSocket connection

`main.go:proxyRoomWS` still hijacks the browser-facing connection so the lobby can return the `101 Switching Protocols` response expected by the gateway. It now uses `gorilla/websocket.Dialer` for the lobby-to-room connection instead of manually constructing a raw TCP handshake and using `io.Copy` on the underlying socket.

This is important because Gorilla retains bytes buffered during the target handshake and handles WebSocket control frames. The proxy does not bypass that buffered reader by reading the underlying `net.Conn` directly.

The browser-to-room direction has a protocol-aware relay that:

- requires masked client frames;
- rejects reserved bits, invalid opcodes, invalid control frames, non-canonical lengths, and oversized frames;
- reassembles fragmented text and binary messages;
- forwards ping, pong, and close control frames; and
- limits application messages to 16 MiB.

Browser-facing writes are serialized so concurrent room data/control messages cannot interleave on the hijacked connection.

### Gateway handoff marker

`static/game.js` sends `{"type":"proxy-ready"}` immediately from `WebSocket.onopen`. The lobby consumes this marker and does not forward it to the room. It uses the first post-upgrade browser frame as the readiness signal before releasing room-to-browser frames; a 500 ms fallback keeps non-browser WebSocket clients from waiting forever.

The marker is harmless in local mode because local mode connects directly to the in-process room handler. Focused tests in `main_test.go` verify marker suppression, fragmented-message reassembly, frame validation, and server-frame payload lengths.

## Verification Status

The following checks passed before this handoff was prepared, and should be rerun after any further proxy change:

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- static Go builds for the API and room images
- local Playwright: 12/12
- direct WebSocket checks against the API service
- repeated gateway-path WebSocket checks with both `joined` frames
- isolated Chromium two-player checks through the NGINX k3d gateway

The full k3d Playwright suite is now green when the gateway NodePort is exposed correctly; the verification details and the earlier misleading failure mode are recorded below.

The failure was ultimately isolated to the test surface. `8080:80@loadbalancer` reached the cluster's default ingress on port 80 rather than the application's `pong-gateway` NodePort 30080, and stale k3d load-balancer state plus intermittent `kubectl port-forward` errors produced misleading failures. With a clean cluster, direct API access passed 30/30, the NGINX gateway path passed 30/30, five isolated Chromium two-player runs passed, and the full k3d suite passed 12/12.

## Build and Deploy with Podman

Podman commonly prefixes local image names with `localhost/`; the Kubernetes manifests intentionally use that prefix and `imagePullPolicy: Never`.

```bash
podman build -t localhost/cloudnativepong-api:latest -f Dockerfile.api .
podman build -t localhost/cloudnativepong-room:latest -f Dockerfile.room .
podman build -t localhost/cloudnativepong-static:latest -f Dockerfile.static .
podman build -t localhost/cloudnativepong-gateway:latest -f Dockerfile.gateway .

k3d image import localhost/cloudnativepong-api:latest \
  localhost/cloudnativepong-room:latest \
  localhost/cloudnativepong-static:latest \
  localhost/cloudnativepong-gateway:latest -c pong

kubectl apply -f k8s/all.yaml
kubectl -n pong wait --for=condition=ready pod -l app=cloudnativepong --timeout=60s
```

For a clean local cluster:

```bash
k3d cluster delete pong 2>/dev/null || true
# Map host port 8080 to pong-gateway's NodePort 30080 on agent 0.
# Do not map 8080 to container port 80: that reaches the default ingress.
k3d cluster create pong --agents 2 --port 8080:30080@agent:0
```

Use the NodePort mapping from the cluster command above for the normal k3d run. Do not use `8080:80@loadbalancer`: port 80 is the cluster's default ingress, not `pong-gateway`. If a port-forward is required for diagnosis, use a dedicated host port and do not combine it with the NodePort path.

## Test Commands

```bash
# Local mode
npx playwright test --reporter=list

# Kubernetes mode, after the gateway NodePort is exposed on host port 8080
TEST_MODE=k8s npx playwright test --reporter=list
```

Before an isolated k3d run, remove stale dynamic room resources and confirm the gateway/API/static pods are ready:

```bash
kubectl -n pong delete pods,svc -l role=room --ignore-not-found
kubectl -n pong get pods,svc
kubectl -n pong logs deploy/pong-gateway --tail=50
kubectl -n pong logs deploy/pong-api --tail=50
```

## Follow-up / Future Hardening

The acceptance issue is resolved as a k3d access-path problem, and no additional proxy timing change is justified by the current evidence. The immediate-`joined` regression is covered by `TestProxyRoomWSForwardsImmediateJoinedFrame` in `main_test.go`.

Future hardening work is deliberately separate from this fix:

1. Add room lifecycle cleanup so completed test and production rooms do not accumulate pods, Services, and database rows.
2. Consider replacing the browser `proxy-ready` application marker with a fully standard WebSocket handoff if a gateway implementation can preserve the same ordering guarantee without the custom lobby hijack.
3. Keep direct API stress, gateway stress, isolated Chromium, and full k3d Playwright checks in the verification matrix when changing the proxy.
4. Repeat the clean-cluster 12/12 k3d run after any gateway, k3d, or port-mapping change.

## Repository Notes

- Room pods and services can accumulate; clean them between k3d runs until lifecycle cleanup is implemented.
- The API uses the Kubernetes API to create room pods and services in the `pong` namespace.
- CI builds with Docker, while local development uses Podman. CI may need image names without Podman's `localhost/` prefix if its manifest convention changes.
- `gateway/nginx.conf` is now the authoritative gateway configuration. Do not restore `gateway/Caddyfile` unless the gateway decision is intentionally revisited.
