# Multi-stage build: Go binary on Alpine, run on scratch.
# Compatible with both Docker and Podman.
#
# Build:
#   podman build -t cloudnativepong:latest .
#
# Run (local mode):
#   podman run --rm -p 8080:8080 cloudnativepong:latest
#
# Run (lobby mode on K8s):
#   podman run --rm -p 8080:8080 cloudnativepong:latest --mode=lobby

# ---- Build Stage ----
FROM docker.io/golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /pong .

# ---- Final Stage ----
FROM scratch

# Timezone data for proper log timestamps
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
# CA certs for K8s API calls
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Static files
COPY static/ /static/
# Binary
COPY --from=builder /pong /pong

EXPOSE 8080
ENTRYPOINT ["/pong"]