# Cloud Native Pong — Handoff Document

## Status

This document describes the gateway/proxy changes prepared for push on the `main` branch. The implementation is materially improved and locally verified, but the full k3d Playwright run still has one intermittent two-player failure. Do not report the k3d path as completely fixed until the follow-up plan below is completed.

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

The full k3d Playwright suite is **not yet consistently green**. The latest repeated result remains 11/12, with the two-player test failing at `tests/e2e.spec.ts:102`: Player 1 remains at `Waiting for opponent...` instead of receiving `Player 1`. Room logs show a Player 1 join followed by a disconnect, while NGINX reports no configuration error. This means the gateway replacement and readiness marker have improved the path but have not established a deterministic end-to-end guarantee.

The failures have also coincided with unstable test plumbing, including stale processes on host port 8080, degraded k3d load-balancer state, and intermittent `kubectl port-forward` errors such as broken pipes and port-forward error-stream timeouts. These infrastructure effects must be separated from an application-level race before changing the proxy again.

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
k3d cluster create pong --agents 2 --port 8080:80@loadbalancer
```

Prefer one stable access path for a test run. The k3d load balancer maps host port 8080 to the gateway; do not simultaneously run a conflicting `kubectl port-forward` on that port.

## Test Commands

```bash
# Local mode
npx playwright test --reporter=list

# Kubernetes mode, after selecting one stable gateway access path
TEST_MODE=k8s npx playwright test --reporter=list
```

Before an isolated k3d run, remove stale dynamic room resources and confirm the gateway/API/static pods are ready:

```bash
kubectl -n pong delete pods,svc -l role=room --ignore-not-found
kubectl -n pong get pods,svc
kubectl -n pong logs deploy/pong-gateway --tail=50
kubectl -n pong logs deploy/pong-api --tail=50
```

## Follow-up Plan — Do Not Treat as Completed Yet

1. **Stabilize the test surface.** Recreate or repair the `pong` cluster, reserve a dedicated host port, remove stale port-forward processes, and use either the k3d load balancer or one port-forward for the entire run—not both. Confirm gateway, API, and static readiness before creating rooms.
2. **Add connection-correlated diagnostics.** Give each proxy attempt a short connection ID and log the browser upgrade, target dial/retry, target `joined` receipt, readiness-marker receipt, first browser write, and close reason. Capture gateway, API, and room logs for the same failing room.
3. **Separate layers with repeatable tests.** Run a direct API-service WebSocket stress test, an NGINX gateway stress test, an isolated Chromium two-player test, and then the full Playwright suite. Repeat each enough times to distinguish a room-startup failure, a proxy ordering failure, and test-environment failure.
4. **Replace timing with a deterministic handoff if needed.** Compare the proxy with Caddy's buffered `brw` handoff pattern and with a standard WebSocket reverse-proxy path. If the readiness marker remains necessary, replace the 500 ms fallback with a protocol/design guarantee or document the client contract explicitly. Avoid adding arbitrary sleeps or retries as the primary fix.
5. **Add an integration regression test.** Simulate a room that sends `joined` immediately after its `101` response and assert that the browser-facing proxy always delivers it, including when the gateway has buffered bytes around the upgrade.
6. **Rebuild and rerun the complete matrix.** Rebuild/import API, room, static, and gateway images with Podman; run Go tests, local Playwright, direct WebSocket stress, isolated Chromium, and full k3d Playwright. The acceptance target is 12/12 locally and 12/12 through k3d across repeated clean runs.
7. **Only then simplify or finalize.** Review whether the custom hijack/relay can be removed in favor of a standard gateway route or whether it is sufficiently deterministic, update this handoff, and make a separate follow-up commit.

## Repository Notes

- Room pods and services can accumulate; clean them between k3d runs until lifecycle cleanup is implemented.
- The API uses the Kubernetes API to create room pods and services in the `pong` namespace.
- CI builds with Docker, while local development uses Podman. CI may need image names without Podman's `localhost/` prefix if its manifest convention changes.
- `gateway/nginx.conf` is now the authoritative gateway configuration. Do not restore `gateway/Caddyfile` unless the gateway decision is intentionally revisited.
