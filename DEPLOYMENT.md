# Cloud Native Pong server deployment

This repository contains both the application and its GitOps deployment for the
experimental server named `vmi3474918`. Platform-level failure drills and rollback commands are
maintained in the [GitOps game-day runbook](https://github.com/macel94/belacca-gitops/blob/main/docs/GAME-DAY-DRILLS.md);
the backup/object-storage contract is in the [GitOps backup contract](https://github.com/macel94/belacca-gitops/blob/main/docs/BACKUP-CONTRACT.md).

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
- The API exposes `/metrics` as aggregate Prometheus text with no labels. The
  metric names cover HTTP outcomes, room lifecycle, Pod/Service orchestration,
  SQLite operations, admission, WebSocket lifecycle, callbacks, and cleanup.
- Each HTTP response includes server-generated `X-Request-ID` and
  `X-Correlation-ID` values. They are opaque 128-bit hex IDs; invalid inbound
  values are replaced. IPs, names, tokens, room IDs, URLs, and request bodies
  are not logged or exported.

## GitOps and ingress layout

Flux v2.9.3 is installed in the `flux-system` namespace. The cluster-level
source controller watches
[`macel94/belacca-gitops`](https://github.com/macel94/belacca-gitops) over HTTPS
using the `flux-system` Secret. That platform repository declares this
repository as an independent child source:

- Platform repository: `https://github.com/macel94/belacca-gitops.git`
- Application repository: `https://github.com/macel94/cloudnativepong.git`
- Branch: `main`
- Flux application path: `./k8s/overlays/server`
- Source refresh interval: 1 minute
- Application reconciliation interval: 10 minutes (force it for immediate validation with `flux reconcile kustomization pong -n flux-system --with-source`)

The Flux controllers are `source-controller`, `kustomize-controller`,
`helm-controller`, and `notification-controller`. The generated Flux
bootstrap manifests live under `clusters/vmi3474918/flux-system` and should be
changed only through the documented Flux upgrade/bootstrap process.

Traefik is the existing k3s ingress controller in `kube-system`. The
cluster-level platform repository owns the host-based Pong Ingress for
`pong.belacca.com`, using class `traefik` and the `web,websecure` entrypoints.
It forwards all paths to `pong-gateway`; NGINX in that gateway handles static
files, API calls, and WebSocket upgrades. The k3d load balancer maps:

- Host `80` and `18080` → Traefik HTTP port 80
- Host `443` → Traefik HTTPS port 443
- Host `18083` → the legacy Pong NodePort 30080
- Host `45371` → the Kubernetes API

The GitOps ingress is the intended application path. The room Pod callbacks at
`/internal/rooms/<id>/started` and `/internal/rooms/<id>/finished` are only
reachable through the `pong-api` ClusterIP Service; the gateway does not route
that path publicly. Callback requests carry only the opaque correlation headers;
callback retries are bounded, and a failed start callback closes the room path
so reconciliation can clean it rather than leaving a half-started game.

The public endpoints are:

```text
https://pong.belacca.com/       → Cloud Native Pong
https://francesco.belacca.com/  → personal site
https://belacca.com/            → redirect to the personal site
https://www.belacca.com/        → redirect to the personal site
```

DNS A records for the two subdomains and both existing apex names point at
`169.58.97.73`. Traefik exposes the `websecure` entrypoint on public port 443
and obtains certificates from Let's Encrypt using the committed Cloudflare
DNS-01 configuration. The out-of-band `kube-system/traefik-cloudflare` Secret
must provide `CLOUDFLARE_DNS_API_TOKEN`; no value is stored in Git. The
certificate store is persisted in `kube-system/traefik-acme`, so certificates
renew across Traefik restarts. WebSockets use the same HTTPS ingress.

## Project clusters on this server

At the time of the latency investigation, this server has **one Kubernetes
cluster for this project**:

| Context | k3d cluster | Topology | Purpose |
|---|---|---|---|
| `k3d-pong` | `pong` | 1 server, 2 agents, 1 load balancer | Local production-like cluster and Flux target |

The persistent project target is `k3d-pong`; the `pong` namespace contains the
application and `flux-system` contains GitOps. CI may create a disposable k3d
cluster for integration tests. The opt-in restore rehearsal may create only a
new `pong-restore-*` cluster and uses an explicit generated context; it never
attaches to or deletes `k3d-pong`. Do not create or delete clusters during
routine debugging, and do not delete/recreate this cluster: the API uses a
persistent SQLite PVC.

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

## Supply-chain evidence and synthetic checks

The published `sha-<40-hex-commit>` image tags are immutable references to a
commit build, but operators should record the registry `sha256` digest for the
exact deployment. `.github/workflows/publish-images.yml` adds BuildKit SBOM and
`mode=max` provenance attestations while pushing to GHCR. Local `docker build`
or `--load` cannot retain registry attestations.

`.github/workflows/supply-chain.yml` creates local images, uploads CycloneDX
SBOMs, and stores Trivy HIGH/CRITICAL reports. It is report-only by default so
normal CI does not fail because of a newly published advisory; a manually
requested `strict=true` run turns those findings into a gate. There are no
registry credentials or vulnerability exceptions in this repository.

`.github/workflows/sign-images.yml` is an explicit manual hook. Provide an
`IMAGE@sha256:<digest>` input after the image has been pushed. The hook uses
Sigstore keyless Cosign signing with GitHub OIDC and rejects mutable tags. It
requires `id-token: write`, registry access, and the external Sigstore services
at run time; it is intentionally not part of ordinary PR/push CI. Inspect the
published manifest and attached referrers with `docker buildx imagetools inspect
IMAGE@sha256:<digest>` and `cosign tree IMAGE@sha256:<digest>`; use the
repository's `verify-image.sh` with an expected workflow identity regexp before
accepting a signature. `cosign verify-attestation` applies only to Cosign-signed
attestations, not automatically to every BuildKit referrer.

To configure the external Pong journey check, set the repository/organization
variable `SYNTHETIC_PONG_URL` and optional `SYNTHETIC_AUTH_TOKEN` secret outside
the repository. The scheduled workflow then checks health, room CRUD, both
WebSocket player assignments, and state delivery. With no URL it performs an
explicit safe skip. It is not a substitute for a separately managed alerting
or paging service.

## SQLite backup and isolated restore rehearsal

The only application state is `/data/pong.db` on PVC `pong-api-data`; no S3,
GCS, bucket, snapshot controller, retention policy, encryption key, or
off-cluster backup destination is configured here. The names-only future
object-storage contract is maintained in
`belacca-gitops/docs/BACKUP-CONTRACT.md`; it is an external prerequisite, not a
provisioned service. `scripts/backup-restore.py` uses SQLite's online backup
API, verifies `PRAGMA integrity_check`, and restores only into a temporary
directory. Run the self-test and verify a protected local artifact before
relying on it:

```bash
./scripts/backup-restore.sh self-test
./scripts/backup-restore.sh backup /path/to/pong.db ./artifacts/pong-$(date -u +%Y%m%dT%H%M%SZ).db
./scripts/backup-restore.sh verify ./artifacts/pong-<timestamp>.db
```

A safe operator-controlled copy from the live PVC is intentionally manual:

1. Confirm a maintenance window, notify users, and record the current Flux
   revision/image digests. Do not delete the cluster, PVC, or namespace.
2. Scale `pong-api` to zero and wait for its pod to terminate so the
   ReadWriteOnce claim is not mounted by two pods.
3. Create a temporary **non-production** helper pod in `pong` that mounts only
   `pong-api-data` at `/data`, wait for it to be Ready, and use `kubectl cp`
   to copy `/data/pong.db` to a protected local path. Delete the helper pod.
4. Run `scripts/backup-restore.sh backup` and `verify` against that local copy.
   For a restore rehearsal, restore into a temporary file or separate
   disposable k3d cluster/PVC only; never overwrite `/data/pong.db` in place.
5. Scale `pong-api` back to one, wait for readiness, and verify the public
   HTTP/API endpoint plus the two-player synthetic journey. Return control to
   Flux rather than leaving a manual deployment change in place.

The procedure produces a verified local artifact but does not pretend it is an
independent backup until an operator copies it to an approved protected
location out-of-band. For a full rehearsal, use the guarded runner rather than
hand-writing a second cluster procedure:

```bash
./scripts/restore-rehearsal.sh self-test
./scripts/restore-rehearsal.sh --backup /protected/path/pong.db \
  --build-images \
  --i-understand-this-creates-an-isolated-cluster
```

The runner requires a new `pong-restore-*` cluster name, verifies the source
before creating it, compares the copied PVC file hash, and checks the restored
API through a localhost-only gateway. It never uses a current kubeconfig
context for Kubernetes operations, never uploads data, and refuses
`k3d-pong`/`pong`. `--keep-cluster` is available for inspection; otherwise it
cleans only the exact disposable cluster it created. The existing `k3d-pong`
cluster, `pong-api-data` PVC, and production `/data/pong.db` must not be
destroyed or overwritten for this rehearsal.

## Observability, failure handling, and bounded smoke

Scrape `/metrics` through the existing private/cluster monitoring path. Do not
add labels or relabel rules containing room IDs, names, IPs, tokens, URLs, or
request IDs. Alerting should use the aggregate success/failure counters and
active/waiting/playing gauges; request IDs are for short-lived request
correlation only and are not a metric dimension.

The orchestration contract is intentionally conservative:

- a Pod/Service create failure removes the database reservation when cleanup is
  confirmed; quota/admission rejection is surfaced as a bounded HTTP failure;
- a resource deletion failure retains the room row so a later reconciliation or
  restart can retry it;
- terminal Pods and Services without a live matching Pod are candidates for
  idempotent cleanup; active playing rooms are retained;
- SQLite remains one-writer and room capacity remains atomically limited to two.

Run the dependency-light harness locally or against a controlled test target.
It caps iterations at 50, concurrency at 8, per-operation timeout at 30 seconds,
and total duration at 3 minutes. It reports aggregate latency percentiles and
failure codes only:

```bash
./scripts/load-smoke.sh --dry-run
LOAD_SMOKE_BASE_URL=http://127.0.0.1:8080 \
  ./scripts/load-smoke.sh --iterations=5 --concurrency=2
```

Use the existing synthetic script for the public workflow and configure its URL
out of band. Do not run sustained load against the public endpoint without an
approved window; the harness is deliberately resource-safe, not an unbounded
load generator.

## Useful checks

```bash
kubectl -n pong get deploy,pods,svc,hpa,pvc
kubectl -n pong get pods -l role=room -o wide
flux get sources git -A
flux get kustomizations -A
```
