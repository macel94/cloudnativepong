# Agent instructions: cloudnativepong

This repository owns Pong application source, Dockerfiles, tests, and application Kubernetes overlays. Read [`belacca-platform/docs/gitops-delivery.md`](https://github.com/macel94/belacca-platform/blob/main/docs/gitops-delivery.md) and [`DEPLOYMENT.md`](DEPLOYMENT.md) before changing production-facing files.

## Safe delivery path

1. Use local mode or a disposable isolated Kubernetes target for development; never use native production or the retired `k3d-pong` target as a sandbox.
2. Run focused tests, then `go test ./...`, `go test -race ./...`, `go vet ./...`, and relevant Playwright/synthetic checks before handoff.
3. Commit and push the source change to `main`.
4. Wait for `.github/workflows/publish-images.yml` to publish immutable GHCR images, attestations, and the generated `deploy: publish images ...` commit. That workflow updates both `k8s/overlays/native-staging` (live native target) and the compatibility server overlay.
5. Fetch the generated commit, reconcile Flux source `cloudnativepong` and Kustomization `pong`, then verify every affected image digest, rollout, health endpoint, and the external two-player journey.

Do not manually `kubectl apply`, `set image`, or edit generated image pins in `belacca-gitops` for normal releases. Update the parent submodule pointer only after the generated deployment commit is on `origin/main` and only when the workspace should track it. Flux `Signature: none` is expected until a reviewed signed-commit verification setup exists.
