# Cloud Native Pong server deployment

Flux reconciles `k8s/overlays/server` into the `pong` namespace.

The overlay expects the four application images and the dynamic room image to
be published to GHCR with an immutable `sha-*` tag. The publish workflow
updates the image tag in `k8s/overlays/server/kustomization.yaml` and the room
image references in the production patches.

The current k3d server exposes Traefik HTTP on host ports `80` and `18080`,
and HTTPS on host port `443`. Host routing is owned by
[`macel94/belacca-gitops`](https://github.com/macel94/belacca-gitops). The public
application URL is `https://pong.belacca.com/`; the apex names redirect to the
personal site at `https://francesco.belacca.com/`.
