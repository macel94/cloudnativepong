# Cloud Native Pong server deployment

This repository contains the Pong application. Its production workloads are
an independently reconciled child source of the platform
`macel94/belacca-gitops` repository, which owns the native-production Flux
root. The current operational state is:

- **Native production:** `belacca-production` serves players through native
  Traefik on `.73`, `.41`, and `.42`, with Flux-managed Pong and a Longhorn-backed
  single-writer SQLite PVC. Cloudflare DNS is direct round-robin, not
  health-aware failover.
- **Retired old production:** the former `k3d-pong` runtime on `.73` was
  stopped and removed after Pong, GoatCounter, and Dex state handoff. Its
  manifests remain historical reference only.
- **CI and restore:** GitHub Actions and the guarded restore rehearsal use
  disposable k3d clusters only; neither is a production target.

The static frontend image injects the full source commit SHA at build time; the
lobby and game pages display a clickable `sha-<short-sha>` badge linking to that
exact GitHub commit. The metadata asset is served with `Cache-Control: no-store`
so the visible marker tracks the deployed static image rather than a stale
browser cache. Platform-level failure drills and rollback commands are
maintained in the [GitOps game-day runbook](https://github.com/macel94/belacca-gitops/blob/main/docs/GAME-DAY-DRILLS.md);
the backup/object-storage contract is in the [GitOps backup contract](https://github.com/macel94/belacca-gitops/blob/main/docs/BACKUP-CONTRACT.md).

## Runtime model

- `pong-api` runs as one replica because it owns the SQLite database and is the
  single writer.
- The database is persisted at `/data/pong.db` on the `pong-api-data` PVC.
  Current native production uses a Longhorn-backed RWO claim and one API writer.
- `pong-gateway` and `pong-static` start with two replicas and can scale to four
  using the Kubernetes metrics-server.
- Every room is a separate Pod named `pong-room-<room-id>` and a matching
  ClusterIP Service. Room Pods have resource limits and a two-hour deadline.
- Creating a room reserves the creator's slot; joining reserves the second slot.
  The lobby marks a room `playing` only after the room Pod confirms both actual
  room-side WebSocket connections through its internal callback. Public
  WebTransport, when enabled, is bridged to this same room-side contract.
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
- Flux application path: `./k8s/overlays/native-staging` (native production;
  the historical `./k8s/overlays/server` path is retained for compatibility/audit)
- Source refresh interval: 1 minute
- Application reconciliation interval: 10 minutes (force it for immediate validation with `flux reconcile kustomization pong -n flux-system --with-source`)

The Flux controllers are `source-controller`, `kustomize-controller`,
`helm-controller`, and `notification-controller`. The platform repository owns
the generated Flux bootstrap manifests and old-production root; change them
only through the documented Flux upgrade/bootstrap process.

Native Traefik is the current public ingress on `.73`, `.41`, and `.42`. The
cluster-level platform repository owns the host-based Pong Ingress for
`pong.belacca.com`, using class `traefik` and the `web,websecure` entrypoints.
It forwards all paths to `pong-gateway`; Caddy in that gateway handles static
routing, API calls, and WebSocket upgrades. Native WebTransport is implemented
in the Go API but remains opt-in because the current HTTP ingress does not
expose UDP.

The GitOps ingress is the current public application path. The room Pod
callbacks at `/internal/rooms/<id>/started` and `/internal/rooms/<id>/finished`
are only reachable through the `pong-api` ClusterIP Service; the gateway does
not route that path publicly. Callback requests carry only the opaque
correlation headers; callback retries are bounded, and a failed start callback
closes the room path so reconciliation can clean it rather than leaving a
half-started game.

Native Traefik on `.73`, `.41`, and `.42` is the public ingress. Native TLS uses
cert-manager DNS-01 and namespace-local Secrets. Monitor all direct-DNS edges
and remove an unhealthy address manually until a health-aware load balancer is
available.

The canonical platform site inventory is maintained in
[`macel94/belacca-gitops/docs/SITES.md`](https://github.com/macel94/belacca-gitops/blob/main/docs/SITES.md).
The Pong application is served at `https://pong.belacca.com/`; the canonical
personal site is `https://francesco.belacca.com/`; and
`belacca.com`, `www.belacca.com`, and `www.francesco.belacca.com` permanently
redirect to that personal site.

Cloudflare DNS-only records for supported application hosts and
`k3s-api.belacca.com` contain `.41` and `.42`. Native cert-manager uses the
out-of-band Cloudflare credential for DNS-01 and stores issued certificates in
namespace-local Kubernetes Secrets. The retired old ACME PVC was not copied or
mounted into native Traefik. Native
WebTransport requires a separate UDP-capable public service, matching TLS
certificate, and `PONG_WEBTRANSPORT_PUBLIC_URL`; it remains disabled until
those platform prerequisites are reviewed.

The image-publish workflow updates both `k8s/overlays/native-staging/` (the
live native Flux target) and the retained server overlay. A source push may
briefly leave those two overlays on different immutable releases until the
publisher's generated `deploy: publish images ...` commit lands; CI validates
each overlay strictly during that short publication window, but generated
deployment commits must pass the full same-release contract. The publisher is
serialized per branch, rejects stale source commits, and retries branch
advances so it cannot overwrite newer application changes. Verify the native
overlay tag, Flux source/kustomization revision, static build marker, and
served JavaScript after reconciliation; successful image publication alone
does not prove that production has rolled out.

## Project clusters and operational state

| Context | Cluster/target | Topology or access | Purpose | Status |
|---|---|---|---|---|
| `belacca-production` | Native k3s cluster | Traefik on `.73`, `.41`, and `.42` | Public production and Flux target | Running; serves players |
| retired `k3d-pong` | historical k3d `pong` on `.73` | Removed containers | Audit/reference only | Retired after state handoff |
| generated CI context | disposable k3d | Job-local gateway | Kubernetes integration tests | Created and deleted per workflow run |
| capacity experiment context | `cnp-capacity-*` k3d | Loopback-only gateway | Bounded capacity baseline | Manual, serialized, redacted evidence, disposable |
| generated restore context | `pong-restore-*` k3d | Explicit isolated context | Restore rehearsal only | Opt-in and disposable |

The retired `k3d-pong` cluster no longer owns public traffic. Native
`belacca-production` is the live Flux target; the platform repository consumes
this repository through its native application Kustomization. The historical
`./k8s/overlays/server` path remains retained for audit/compatibility, while
`k8s/overlays/test` and the `cnp-capacity-*` workflow are disposable-only. Do
not create or delete clusters during routine debugging, and do not
delete/recreate native production or its SQLite PVC.

### Native storage and single-writer operations

Native production uses a Longhorn-backed RWO claim and one API writer. The
following controls are required for state operations:

1. Install Longhorn on the native cluster and verify healthy replicas,
   StorageClasses, backups/snapshots, node scheduling, and restore behavior.
2. Create and test a native Longhorn-backed claim with the same single-writer
   expectation as `pong-api-data`. The API must remain one replica; never mount
   the live SQLite database read/write from both clusters.
3. Take and verify an offline/online SQLite backup from the legacy PVC during a
   maintenance window. Stop the legacy API before any file-level copy so the
   `local-path` RWO claim has one writer and no concurrent mount.
4. Restore the verified copy into an isolated target or approved native
   maintenance window, run integrity and application checks, and preserve the
   single-writer contract.
5. Keep Cloudflare application records limited to healthy native edge
   addresses (`.73`, `.41`, `.42`) until a
   health-aware load balancer exists; remove an unhealthy address manually.

## Fastest safe debug, local test, and GitOps workflow

The default inner loop is local process mode. See the workspace [fast
development-loop guide](https://github.com/macel94/belacca-platform/blob/main/docs/development-loop.md)
for the separation between local development, an isolated Kubernetes
development plane, and reviewed production GitOps promotion. Do not use the
public `k3d-pong` cluster as a repeated experiment sandbox.

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
4. **Validate the full architecture only in an isolated environment.** Until
   the separate `pong-dev` interception/container workflow is provisioned, use
   an explicitly disposable development cluster or another operator-approved
   isolated environment. Build only the changed images, export/import them,
   and patch only that environment. Never patch the public `k3d-pong` cluster
   for routine experiments. Wait for the affected rollout, run the focused
   Kubernetes Playwright test, and capture logs, events, resource usage,
   real-time frame cadence, and room cleanup.
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
   two-player WebSocket-compatible game, and no unexpected room Pods or Services remain.

This workflow keeps rapid debugging local, tests the actual Kubernetes real-time
path before merge, and ensures production changes arrive through Git history
rather than drift from manual `kubectl apply` operations.

## Latency investigation: real-time frame delivery

The observed lag was caused by bursty delivery of small authoritative state
frames through the multi-hop real-time path, not by node saturation: node CPU
remained below 20% and application pods used little CPU. The browser previously
rendered the ball and opponent from a deliberate 50ms historical interpolation
window, making network delay visible as paddle/ball lag. Online rendering now
uses the newest snapshot immediately, advances the ball from its authoritative
velocity and the opponent from recent velocity on `requestAnimationFrame`, and
smoothly decays bounded corrections when snapshots arrive. The server remains
authoritative; presentation extrapolation does not alter gameplay decisions.

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

The packages are configured for anonymous pulls. The workflow updates both the live native-staging overlay and the retained
server overlay with `sha-<40-hex-commit>` tags and commits the deployment change
back to the feature branch. A future merge to `main` should use the same
workflow after the production branch policy is chosen.

## Supply-chain evidence and synthetic checks

The published `sha-<40-hex-commit>` image tags are immutable references to a
commit build, but operators should record the registry `sha256` digest for the
exact deployment. `.github/workflows/publish-images.yml` adds a registry SBOM
and GitHub Artifact Attestation SLSA provenance while pushing to GHCR. Local
`docker build` or `--load` cannot retain registry attestations.

`.github/workflows/supply-chain.yml` creates local images, uploads CycloneDX
SBOMs, stores Trivy HIGH/CRITICAL reports, and validates the immutable
`release-metadata.json` contract. `scripts/validate-release.py` fails closed if
production references are not full SHA tags or promoted digests. The
`scripts/promote-digests.sh` helper requires four exact GHCR digest references;
use `--verify-attestations` to run repository/workflow-scoped GitHub Artifact
Attestation checks before metadata becomes fully `verified`. Without that flag,
the safe state is `digests_resolved`. It is report-only by default so normal CI
does not fail because of a newly published advisory; a manually requested
`strict=true` run turns those findings into a gate. There are no registry
credentials or vulnerability exceptions in this repository.

`.github/workflows/publish-images.yml` uses `actions/attest@v4` with
`id-token: write`, `attestations: write`, and `artifact-metadata: write`. Each
pushed GHCR image receives GitHub-signed SLSA provenance in the registry. Verify
one immutable image with:

```bash
./scripts/verify-attestation.sh \
  ghcr.io/macel94/cloudnativepong-api@sha256:<digest>
```

The helper runs `gh attestation verify` with the expected repository,
publish workflow, OIDC issuer, GitHub attestation record, and SLSA provenance predicate.
This is the native GitHub path; no separate signing executable, key, or manual
signing workflow is required.

The scheduled/manual Pong journey check runs against
`https://pong.belacca.com` by default. An approved alternate ingress may be
provided through the out-of-band repository/organization variable
`SYNTHETIC_PONG_URL`; `SYNTHETIC_AUTH_TOKEN` is optional and is only needed for
a protected front door. The runner checks the homepage, health, room CRUD,
exact connection contract, both WebSocket player assignments, playing state,
and room disappearance after disconnect. Missing configuration, an HTTP or
WebSocket failure, a timeout, or failed cleanup is a failed check—not a green
skip. It is not a substitute for separately managed alerting or paging.

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

## Observability, failure handling, capacity, and bounded smoke

### Optional OpenTelemetry tracing

Pong includes OpenTelemetry Go `v1.45.0` tracing with OTLP/HTTP export. It is
opt-in: without an OTLP endpoint, the process uses the SDK no-op provider and
has no collector dependency or export traffic. Enable it only after an
approved private collector is available:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-collector.example.invalid:4318 \
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://otel-collector.example.invalid:4318/v1/traces \
  ./cloudnativepong --mode=local
```

The endpoint must be supplied out of band and must use TLS unless the operator
explicitly sets the documented OTLP insecure variable for a private local test.
The process flushes and shuts down the provider within a bounded timeout.
HTTP spans use only normalized route names, methods, and status codes. W3C
trace context is propagated to internal room callbacks and proxy WebSocket
connections. Room IDs, player names, client IPs, tokens, arbitrary URLs, and
request bodies are never span attributes.

The implementation follows the official OpenTelemetry Go API/SDK and OTLP/HTTP
exporter documentation:

- <https://opentelemetry.io/docs/languages/go/>
- <https://opentelemetry.io/docs/languages/go/exporters/>
- <https://opentelemetry.io/docs/languages/go/instrumentation/>
- <https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp>

Scrape `/metrics` through the existing private/cluster monitoring path. The
machine-readable `slo-contract.json` defines the external 99%/30d journey SLI;
internal metrics are diagnostics and do not establish public availability. Do
not add labels or relabel rules containing room IDs, names, IPs, tokens, URLs,
or request IDs. Alerting should use aggregate success/failure counters,
bounded duration distributions, and active/waiting/playing gauges; request IDs
are for short-lived request correlation only and are not a metric dimension.
The separate controlled-drill recovery objective is P95 under six minutes and
must not be derived from these availability signals.

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

Use the scheduled synthetic script for the public workflow; it is a low-rate
availability check, not a load generator. The availability objective is 99% per
public service over 30 days; the controlled-drill recovery objective is P95 under
six minutes and is separate from availability. The manual `capacity-experiment`
workflow is the supported CI capacity path: it is loopback-only, serialized,
bounded, and redacts uploaded snapshots. Its first 8-concurrent baseline hit the
configured WebSocket admission boundary before CPU/RAM saturation. For local or
disposable load-smoke runs, set `LOAD_SMOKE_BASE_URL` explicitly. Loopback
requires no authorization; every non-local target requires all three explicit
markers: `PONG_EXPERIMENT_MODE=capacity|chaos`, `PONG_EXPERIMENT_APPROVED=1`, and
`PONG_EXPERIMENT_TARGET=isolated`. Canonical public hosts and native edge
addresses are always denied. Playwright is fixed to one worker with no retries.
The capacity and chaos workflows share one global `capacity-chaos-experiment`
lock, have no matrix/retry fan-out, and use run-owned disposable k3d resources.

The chaos workflow runs one selected fault (`api-restart`, `gateway-restart`,
`room-termination`, `node-drain`, or `resource-pressure`) three times after a
passing concurrent-room baseline. It emits aggregate recovery P95, failure,
cleanup, and resource evidence; it does not establish public availability.
This branch has not executed a live cluster drill. An operator must run at
least three comparable approved isolated drills and review P95 against six
minutes; never run them against public production.

## Useful checks

```bash
kubectl -n pong get deploy,pods,svc,hpa,pvc
kubectl -n pong get pods -l role=room -o wide
flux get sources git -A
flux get kustomizations -A
```
