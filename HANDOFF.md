# Cloud Native Pong — GitOps/server deployment handoff

**Checkpoint date:** 2026-07-31
**Branch:** `feat/gitops-server-deployment`
**Purpose:** Preserve a clear continuation point for the Cloud Native Pong deployment work. Do not merge this branch into `main` until the room-lifecycle issue below is fixed and verified.

## Executive status

The GitOps deployment itself is healthy on the existing `vmi3474918` server and `k3d-pong` cluster. Flux is successfully reconciling the feature branch, the application is reachable through Traefik, GHCR-backed immutable images are running, and the existing 12-test Kubernetes Playwright suite previously passed.

The deployment is **not yet complete** because room lifecycle cleanup is not proven. The last full E2E run left two waiting room records, Pods, and Services behind. The root cause is understood, but the follow-up lifecycle code is currently **uncommitted and not deployed** at this checkpoint.

## What is verified

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
- `go test ./...` passed at this checkpoint.
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

## What was tried but is not yet committed/deployed

The working tree contains experimental application changes in `db/db.go` and `lobby/lobby.go`.

### `db/db.go`

`IncrementPlayers` was changed from an unconstrained increment to an atomic SQL update with `players < 2`, followed by explicit `room not found`/`room is full` errors. This addresses concurrent join races.

### `lobby/lobby.go`

The current uncommitted changes:

- add a default 10-minute idle timeout and `NewServerWithIdleTimeout` constructor;
- scan persisted rooms during reconciliation and attempt cleanup for old `waiting` rooms;
- retain active `playing` rooms;
- currently leave `JoinRoom` behavior partly changed from the intended final design and do not yet add room-start notification;
- have not yet changed the internal HTTP routes to support a room-start callback;
- have not yet been covered by focused lobby tests;
- have not yet been built into/published as a container image;
- have not yet been reconciled by Flux.

Do not assume these edits are production-ready merely because `go test ./...` passed; there are no existing lobby package tests, and Kubernetes cleanup behavior still needs live verification.

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

## Next steps, in order

1. Inspect/rework the uncommitted `db/db.go` and `lobby/lobby.go` changes before committing them.
2. Add focused tests for:
   - atomic two-player capacity under concurrent joins;
   - a first join leaving status `waiting`;
   - actual room start changing status to `playing`;
   - idle waiting-room expiration (using a short injected timeout);
   - idempotent cleanup/error handling.
3. Update `main.go` room mode:
   - notify the lobby when both room WebSocket connections are present;
   - signal/notify cleanup on disconnect and write failure where appropriate;
   - ensure room-mode `/health` remains available.
4. Add the started callback route in lobby mode and keep finished cleanup idempotent.
5. Set reconciliation to a one-minute interval and document the actual interval accurately.
6. Run `gofmt`, `go test ./...`, `go test -race ./...`, `go vet ./...`, static builds, Kustomize render/dry-run, and the local Playwright suite.
7. Commit the application lifecycle fix separately from generated image/deployment changes.
8. Push the feature branch and wait for the image workflow to generate the GHCR SHA-tag deployment commit. Fetch the remote branch before making more commits because the workflow may advance it.
9. Force Flux reconciliation and verify all live workloads use the new SHA tag.
10. Remove only the currently leaked room resources through the API/controlled cleanup or, if necessary, targeted `kubectl -n pong delete pod,svc` plus DB cleanup; do not delete the API PVC.
11. Run a fresh Kubernetes Playwright suite through `http://169.58.97.73:18080/`.
12. Create an abandoned room and verify the DB row, Pod, and Service disappear within the documented timeout.
13. Run the two-player WebSocket flow and verify one room Pod/Service exists while active, then cleanup occurs after completion/disconnect.
14. Recheck Flux, HPA, PVC, quota, ingress, image tags, public access, and documentation.
15. Keep the feature branch deployed for validation. Merge to `main` only after the live lifecycle test passes.

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

**Working:** GitOps/Flux reconciliation, immutable images, Traefik HTTP ingress, public HTTP reachability, stateless scaling, single-replica persistent SQLite API, dynamic room Pod creation, and the previously verified two-player WebSocket path.

**Not working/proven:** automatic cleanup of rooms created but never joined; robust cleanup on abandoned WebSocket reservations; final lifecycle implementation and live post-fix verification.

**Working tree note:** this handoff commit documents the current uncommitted lifecycle experiment. The next agent/session must inspect it before implementing further changes.
