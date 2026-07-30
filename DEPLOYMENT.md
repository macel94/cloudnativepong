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

## GitOps layout

Flux watches `clusters/vmi3474918` and reconciles `k8s/overlays/server`.
Images are immutable GHCR tags of the form `sha-<git commit>`; the publish
workflow updates the overlay after building all four images.

The k3d cluster on this server currently exposes Traefik on host port `18080`.
The experimental site is therefore reachable at:

```text
http://169.58.97.73:18080/
```

A DNS record and HTTPS entrypoint can be added later without changing the
application routes. WebSockets are carried through the same gateway ingress.

## Useful checks

```bash
kubectl -n pong get deploy,pods,svc,hpa,pvc
kubectl -n pong get pods -l role=room -o wide
flux get sources git -A
flux get kustomizations -A
```
