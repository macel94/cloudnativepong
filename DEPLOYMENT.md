# Cloud Native Pong server deployment

This repository contains both the application and its GitOps deployment for the
experimental server named `vmi3474918`.

## Runtime model

- `pong-api` runs as one replica because it owns the SQLite database.
- The database is persisted at `/data/pong.db` on the `pong-api-data` PVC.
- `pong-gateway` and `pong-static` start with two replicas and can scale to four
  using the Kubernetes metrics-server.
- Every room is a separate Pod named `pong-room-<room-id>` and a matching
  ClusterIP Service. Room Pods have resource limits and a two-hour deadline.
- Creating a room reserves the creator's slot; joining reserves the second slot.
  The lobby marks a room `playing` only after the room Pod confirms both actual
  WebSocket connections through its internal callback.
- Completed, failed, orphaned, and abandoned waiting-room resources are cleaned
  by the lobby. Waiting rooms expire after 10 minutes, with reconciliation
  running every 1 minute.

## GitOps and ingress layout

Flux v2.9.3 is installed in the `flux-system` namespace. Its source controller
watches this repository over HTTPS using the `flux-system` Secret:

- Repository: `https://github.com/macel94/cloudnativepong.git`
- Branch: `main` (the production source of truth)
- Flux path: `./clusters/vmi3474918`
- Application path: `./k8s/overlays/server`
- Source refresh interval: 1 minute
- Application reconciliation interval: 10 minutes (force it for immediate validation with `flux reconcile kustomization`)

The Flux controllers are `source-controller`, `kustomize-controller`,
`helm-controller`, and `notification-controller`. The generated Flux
bootstrap manifests live under `clusters/vmi3474918/flux-system` and should be
changed only through the documented Flux upgrade/bootstrap process.

Traefik is the existing k3s ingress controller in `kube-system`. The Pong
Ingress uses class `traefik` and the `web` entrypoint, forwarding all paths to
`pong-gateway`; NGINX in that gateway handles static files, API calls, and
WebSocket upgrades. The k3d load balancer maps:

- Host `18080` → Traefik HTTP port 80
- Host `18083` → the legacy Pong NodePort 30080
- Host `45371` → the Kubernetes API

The GitOps ingress is the intended application path. The room Pod callbacks at
`/internal/rooms/<id>/started` and `/internal/rooms/<id>/finished` are only
reachable through the `pong-api` ClusterIP Service; the gateway does not route
that path publicly. After reconciliation the experimental site is reachable at:

```text
http://169.58.97.73:18080/
```

A DNS A/AAAA record can point at `169.58.97.73` later. HTTPS requires adding a
Traefik `websecure` entrypoint and an ACME/Let's Encrypt configuration; it is
not silently enabled by this POC. WebSockets work over the same HTTP ingress
and will also work over HTTPS once TLS is configured.

## Project clusters on this server

At the time of the latency investigation, this server has **one Kubernetes
cluster for this project**:

| Context | k3d cluster | Topology | Purpose |
|---|---|---|---|
| `k3d-pong` | `pong` | 1 server, 2 agents, 1 load balancer | Local production-like cluster and Flux target |

There are no other project contexts or k3d clusters configured on the server.
The `pong` namespace contains the application; `flux-system` contains GitOps.
Do not create a second cluster or delete/recreate this one during routine
debugging: the API uses a persistent SQLite PVC.

## Fastest safe debug, local test, and GitOps workflow

1. **Inventory and baseline without changing anything.**
   ```bash
   git status --short --branch
   kubectl config get-contexts
   k3d cluster list
   kubectl get nodes -o wide
   kubectl -n pong get deploy,pods,svc,ingress,hpa,pvc,resourcequota -o wide
   kubectl top nodes
   kubectl top pods -n pong
   flux get sources git -A
   flux get kustomizations -A
   curl -i http://169.58.97.73/
   ```
2. **Create an isolated branch from the current production commit.** Do not
   manually edit production deployments as the permanent fix, and do not run
   destructive commands against the API PVC.
3. **Use the fastest feedback loop first.** Run `go test ./...`,
   `go test -race ./...`, `go vet ./...`, `node --check static/game.js`, and
   `git diff --check`. Use local mode for application-only changes:
   `go run . --mode=local` followed by `npx playwright test`.
4. **Validate the full architecture in the existing cluster.** Build the
   changed images with Podman, export/import them into k3d, and patch only the
   live test deployment with temporary `localhost/...` image tags and
   `IfNotPresent`. Wait for every rollout, run the Kubernetes Playwright suite,
   and capture logs, events, resource usage, WebSocket cadence, and room cleanup.
   Restore Flux-managed images immediately after the test.
5. **Commit and push the branch.** GitHub Actions builds immutable
   `sha-<commit>` images and commits the generated overlay tag update back to
   the same feature branch. Fetch the branch before any follow-up commit because
   that generated deployment commit may arrive asynchronously.
6. **Review and merge the pull request into `main`.** Flux production tracks
   `main`; after the merge, wait for the image workflow's generated deployment
   commit, then force reconciliation if needed:
   ```bash
   flux reconcile source git flux-system -n flux-system
   flux reconcile kustomization flux-system -n flux-system --with-source
   kubectl -n pong rollout status deployment/pong-api
   kubectl -n pong rollout status deployment/pong-gateway
   kubectl -n pong rollout status deployment/pong-static
   ```
7. **Verify production after reconciliation.** Confirm Flux `Ready=True`, all
   pods Ready, the expected immutable image tags, ingress/API HTTP 200, a
   two-player WebSocket game, and no unexpected room Pods or Services remain.

This workflow keeps rapid debugging local, tests the actual Kubernetes/WebSocket
path before merge, and ensures production changes arrive through Git history
rather than drift from manual `kubectl apply` operations.

## Latency investigation: WebSocket frame delivery

The observed lag was caused by bursty delivery of small authoritative state
frames through the multi-hop WebSocket path, not by node saturation: node CPU
remained below 20% and application pods used little CPU. The browser rendered
only when a network frame arrived, making packet bursts visible as paddle/ball
stutter. The fix enables TCP_NODELAY on Go WebSocket connections and adds a
short client-side interpolation buffer rendered continuously with
`requestAnimationFrame`. The server remains authoritative; interpolation does
not predict or alter gameplay decisions.

Validation on the local `k3d-pong` cluster passed all 12 Kubernetes Playwright
checks after the change. Frame-cadence measurements remain intentionally
network-dependent, so visual smoothness and gameplay correctness are validated
in a real browser rather than by requiring exactly 60 packets per second.

## GitHub Actions and images

`.github/workflows/publish-images.yml` builds four images and pushes immutable
commit tags to:

```text
ghcr.io/macel94/cloudnativepong-api
ghcr.io/macel94/cloudnativepong-room
ghcr.io/macel94/cloudnativepong-static
ghcr.io/macel94/cloudnativepong-gateway
```

The packages are configured for anonymous pulls. The workflow updates the
server overlay with `sha-<40-hex-commit>` tags and commits the deployment change
back to the feature branch. A future merge to `main` should use the same
workflow after the production branch policy is chosen.

## Useful checks

```bash
kubectl -n pong get deploy,pods,svc,hpa,pvc
kubectl -n pong get pods -l role=room -o wide
flux get sources git -A
flux get kustomizations -A
```
