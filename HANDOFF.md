# Cloud Native Pong — historical GitOps/server deployment handoff

> **Superseded:** This checkpoint describes the former single-repository Flux
> setup. The current multi-project hosting plan moves cluster ownership to
> [`macel94/belacca-gitops`](https://github.com/macel94/belacca-gitops), keeps Pong
> at [`pong.belacca.com`](https://pong.belacca.com), and hosts the personal site
> at [`francesco.belacca.com`](https://francesco.belacca.com). The aliases
> `belacca.com`, `www.belacca.com`, and `www.francesco.belacca.com` redirect to
> the canonical personal site. See the canonical
> [platform site inventory](https://github.com/macel94/belacca-gitops/blob/main/docs/SITES.md)
> and use `belacca-gitops/MIGRATION.md` for the staged cutover procedure.

**Checkpoint date:** 2026-07-31
**Branch:** `feat/gitops-server-deployment`
**Purpose:** Preserve historical context for the prior Cloud Native Pong deployment work.

## Executive status

The former GitOps deployment was healthy on the existing `vmi3474918` server and `k3d-pong` cluster. The live cluster has since been observed on `main`; the platform repository migration is documented separately.

This document is retained for lifecycle investigation history only. Do not use its former branch/source instructions for the multi-project cutover.

## What is verified

This historical checkpoint predates the current `main` rollout. The former
follow-up added bounded aggregate application metrics, opaque request/correlation
IDs, cluster-free orchestration failure injection, and the dependency-light
`scripts/load-smoke.sh` harness. The current main branch has since migrated the
HTTP/static edge to Caddy, moved Go runtimes to pinned Distroless images, and
added an opt-in native WebTransport path with a WebSocket-compatible default.

- `main` remains known-good at commit `2c5399c`.
- Feature branch: `feat/gitops-server-deployment`.
- The branch contains Flux bootstrap/application manifests, server Kustomize overlays, immutable image publishing, persistent SQLite storage, Traefik Ingress, and dynamic one-Pod-per-room orchestration.
- Flux source tracks:
  - Repository: `https://github.com/macel94/cloudnativepong.git`
  - Branch: `feat/gitops-server-deployment`
  - Root: `./clusters/vmi3474918`
  - Application overlay: `./k8s/overlays/server`
- Latest observed Flux revision: `e4a8774d` (`deploy: publish images sha-a6bef...`).
- Flux `GitRepository/flux-system`: `Ready=True`.
- Flux root `Kustomization/flux-system`: `Ready=True`, message `Applied revision: feat/gitops-server-deployment@sha1:e4a8774d`.
- Live application images are immutable GHCR SHA tags for commit `a6bef049...`:
  - `ghcr.io/macel94/cloudnativepong-api:sha-a6bef...`
  - matching room, static, and gateway tags.
- Anonymous GHCR pulls were previously verified with HTTP 200; no image pull secret is currently required.
- `go test ./...`, `go test -race ./...`, and `go vet ./...` pass with the lifecycle implementation and focused tests.
- Focused tests cover request-ID validation/headers, HTTP status metrics,
  callback retry/failure behavior, bounded metric names, SQLite legacy
  timestamp restart compatibility, capacity/admission rejection, Pod create
  and deletion failure, terminal Pod cleanup, orphan Services, and restart /
  reconcile behavior without a live cluster.
- `./scripts/load-smoke.sh --dry-run` passes. A real local run with two
  concurrent journeys passed health/create/join/WebSocket/cleanup and left
  zero rooms and zero active WebSockets.
- Earlier verified checks include `go vet ./...`, CGO-free builds, Kustomize rendering, Kubernetes dry runs, local Playwright 12/12, and Kubernetes Playwright 12/12.
- Public/intended URL: `http://169.58.97.73:18080/`.

## Live cluster state at checkpoint

Cluster context: `k3d-pong`

- Nodes: `k3d-pong-server-0`, `k3d-pong-agent-0`, `k3d-pong-agent-1`.
- Host mappings:
  - `18080 -> Traefik HTTP :80` (intended GitOps path)
  - `18083 -> Pong gateway NodePort :30080` (legacy/direct path)
  - `45371 -> Kubernetes API :6443`
- `pong-api`: 1/1, GHCR SHA image, SQLite PVC mounted.
- `pong-gateway`: 2/2, HPA min 2/max 4.
- `pong-static`: 2/2, HPA min 2/max 4.
- `pong-api`: deliberately one replica with SQLite-safe `Recreate` strategy.
- PVC `pong-api-data`: `Bound`, 1 Gi, `local-path`, RWO.
- Ingress `pong`: Traefik class, wildcard host, address on all three nodes.
- Resource quota was healthy: 7/120 Pods and 5/120 Services at inspection time, including two leaked rooms.

## Architecture/decisions

- Flux is used for normal deployment; do not use manual `kubectl apply` for routine rollout.
- API remains one replica because SQLite is persisted but not multi-writer safe.
- Gateway/static are the only conventional HPA targets.
- Every game room is a dedicated dynamically created Pod plus matching ClusterIP Service.
- Room Pods use immutable GHCR images, readiness/liveness `/health` probes, resource requests/limits, `restartPolicy: Never`, and a two-hour `activeDeadlineSeconds`.
- Traefik exposes the application on HTTP. HTTPS is intentionally not enabled; it requires a domain plus Traefik websecure/ACME configuration.
- Do not recreate or destructively replace the existing cluster or PVC.

## What was found during cleanup investigation

The E2E tests create rooms in several tests without joining them. Creating a room creates:

1. a SQLite room row with status `waiting` and zero players;
2. a room Pod; and
3. a matching room Service.

A room Pod only calls the lobby `/internal/rooms/<id>/finished` endpoint after its in-process game signals completion. A room that never receives a WebSocket connection never starts a game and therefore never signals completion. The reconciler only removed terminal Pods, finished DB records, or orphaned Services; it did not expire old waiting rows/resources.

Observed leaked resources after the last test run:

- Pods: `pong-room-0f2afc`, `pong-room-72a65a`
- Services with the same IDs
- both rooms still returned by `/api/rooms`
- four room resources total across Pods and Services
- room logs showed only `Cloud Native Pong starting on :8080 (mode=room)`

This is a real lifecycle bug, not a Flux or ingress failure.

## Historical implementation state (superseded)

The former feature-branch working tree contained experimental application, test,
and documentation changes in `db/db.go`, `lobby/lobby.go`, `main.go`, and related
files. Those changes are now part of the published mainline history.

The application-side implementation is locally verified with the full Go test
suite, race detector, vet, Node syntax/dry-run checks, and the bounded local
smoke harness. It has not yet been built into/published as a container image or
reconciled by Flux.

### `db/db.go`

`IncrementPlayers` was changed from an unconstrained increment to an atomic SQL update with `players < 2`, followed by explicit `room not found`/`room is full` errors. This addresses concurrent join races.

### `lobby/lobby.go`

The historical changes at this checkpoint included:

- add a default 10-minute idle timeout and `NewServerWithIdleTimeout` constructor;
- scan persisted rooms during reconciliation and attempt cleanup for old `waiting` rooms;
- retain active `playing` rooms;
- reserve capacity atomically without changing lifecycle status;
- add an idempotent `MarkRoomPlaying` operation that requires two reservations;
- add a one-minute reconciler and injected idle timeout;
- add `/internal/rooms/<id>/started` alongside the existing finished callback;
- notify the lobby after both actual room WebSockets connect, and signal cleanup on disconnect/write failure;
- add focused DB, lobby, and room-WebSocket tests;
- have not yet been built into/published as a container image;
- have not yet been reconciled by Flux.

The cluster-free tests do not claim live-cluster verification. Kubernetes
cleanup, quota behavior, metrics scraping, callback network policy, image
publication, and Flux reconciliation remain operator gates.

## Important design correction for continuation

A prior attempted approach marked a room `playing` when the second `/api/rooms/join` reservation succeeded. That is unsafe: a client can reserve the second slot and then abandon the page before opening its WebSocket, causing an abandoned room to evade a `waiting`-only timeout.

The safer model is:

1. API join atomically reserves capacity only.
2. The room Pod tracks actual WebSocket connections.
3. Once both players are connected, the room Pod POSTs an internal `started` callback; the API marks the DB room `playing`.
4. If a player disconnects before/during play, the room Pod signals completion and exits/requests cleanup.
5. A room with no players or only one player expires after a bounded idle timeout.
6. Reconciliation runs frequently enough to enforce the bound (target one minute, not five minutes).

The callback route must be authenticated by network/RBAC assumptions or otherwise constrained before production exposure; it is currently an internal ClusterIP-only endpoint.

## Remaining operator gates

1. Build/publish the application image and reconcile it through the intended
   GitOps path; do not stage or commit automatically.
2. Run the final local manifest/rendering and browser checks listed below; the
   application-side Go/race/vet and bounded smoke checks already pass.
3. Publish the image through the reviewed workflow, reconcile it through Flux,
   and verify the live image digest and rollout.
4. Verify `/metrics` scraping, resource quota/admission behavior, callback
   network reachability, abandoned-room cleanup, and post-disconnect cleanup
   on the intended cluster without deleting the existing cluster or PVC.
5. Run the public/cluster Playwright and synthetic journeys only in an approved
   test window, then recheck Flux, HPA, PVC, quota, ingress, image tags, public
   access, and documentation.
6. Do not stage or commit automatically; merge only after the live lifecycle
   and deployment checks pass through the normal review process.

## Useful commands

```bash
# Branch and uncommitted state
git status --short --branch
git fetch origin feat/gitops-server-deployment
git log --oneline --decorate -8 origin/feat/gitops-server-deployment

# Tests
gofmt -w db/db.go lobby/lobby.go main.go
go test ./...
go test -race ./...
go vet ./...

# Flux and workloads
flux get all -A
flux reconcile source git flux-system -n flux-system
flux reconcile kustomization flux-system -n flux-system --with-source
kubectl -n pong get deploy,pods,svc,hpa,ingress,pvc,resourcequota -o wide
kubectl -n pong get pods,svc -l role=room -o wide

# Public smoke test
curl -i http://169.58.97.73:18080/
curl -i http://169.58.97.73:18080/api/rooms

# Kubernetes E2E
TEST_MODE=k8s BASE_URL=http://169.58.97.73:18080 npx playwright test --reporter=list
```

## Current checkpoint conclusion

This file remains a historical lifecycle handoff and is not the current deployment
source of truth. The published main branch now includes Caddy gateway/static
images, pinned Distroless API/room runtimes, native WebTransport support behind an
explicit UDP/TLS configuration, and a WebSocket-compatible default path. The
current deployment and operational documentation live in `README.md` and
`DEPLOYMENT.md`; production promotion still occurs through the application
publish workflow and the parent GitOps repository.
