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
- Completed, failed, and orphaned room resources are cleaned by the lobby.

## GitOps and ingress layout

Flux v2.9.3 is installed in the `flux-system` namespace. Its source controller
watches this repository over HTTPS using the `flux-system` Secret:

- Repository: `https://github.com/macel94/cloudnativepong.git`
- Branch: `feat/gitops-server-deployment` during this staged rollout
- Flux path: `./clusters/vmi3474918`
- Application path: `./k8s/overlays/server`
- Reconciliation interval: 1 minute for the application overlay

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

The GitOps ingress is the intended application path. After reconciliation the
experimental site is reachable at:

```text
http://169.58.97.73:18080/
```

A DNS A/AAAA record can point at `169.58.97.73` later. HTTPS requires adding a
Traefik `websecure` entrypoint and an ACME/Let's Encrypt configuration; it is
not silently enabled by this POC. WebSockets work over the same HTTP ingress
and will also work over HTTPS once TLS is configured.

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
