# Cloud Native Pong server deployment

Flux reconciles `k8s/overlays/server` into the `pong` namespace.

The overlay expects the four application images and the dynamic room image to
be published to GHCR with an immutable `sha-*` tag. The publish workflow
updates the image tag in `k8s/overlays/server/kustomization.yaml` and the room
image references in the production patches.

The current k3d server exposes Traefik on host port `18080`.
