# Generic multi-stage build for every Go service under cmd/ — build with
# --build-arg SERVICE=<name> matching a cmd/<name> directory, e.g.:
#   docker build --build-arg SERVICE=gateway -f deploy/docker/Dockerfile -t gateway .
#
# One Dockerfile for all 14 services (see Makefile's SERVICES list) rather
# than 14 near-duplicates — every cmd/* binary shares the same build
# (CGO_ENABLED=0, static) and runtime shape (deploy/k8s/base/services/*.yaml
# all run as UID 65534, read-only root filesystem, no shell).
#
# grpc-health-probe is built into every image, not just the gRPC services:
# 12 of the 14 k8s Deployments use `exec: grpc-health-probe -addr=:9090` for
# their readiness/liveness probes (see pkg/grpcutil/server.go's NewServer
# comment) — that binary must exist *inside* the container for an exec
# probe to find it. The couple of services that don't need it (gateway
# uses httpGet; seed is a run-to-completion Job with no probes) still get
# it for a few extra MB, in exchange for one shared, simpler Dockerfile.
ARG GO_VERSION=1.26-bookworm
FROM golang:${GO_VERSION} AS builder

ARG GRPC_HEALTH_PROBE_VERSION=v0.4.54

WORKDIR /src

# Installed before ARG SERVICE is declared and before COPY . . : BuildKit
# folds every currently-in-scope ARG into a RUN layer's cache key even when
# the command doesn't reference it, so SERVICE must not be declared yet
# here — otherwise this step would silently re-download on every distinct
# `--build-arg SERVICE=...` build instead of being built once and reused.
RUN CGO_ENABLED=0 GOOS=linux GOBIN=/out go install \
    "github.com/grpc-ecosystem/grpc-health-probe@${GRPC_HEALTH_PROBE_VERSION}"

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG SERVICE
RUN test -d "./cmd/${SERVICE}" || (echo "no such service: cmd/${SERVICE}" >&2 && exit 1)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app "./cmd/${SERVICE}"

# static-debian12 (not base-debian12): every service binary above is fully
# static (CGO_ENABLED=0), and static-debian12 still carries ca-certificates
# + tzdata, needed for outbound TLS calls (e.g. ai-assistant's LLM provider,
# objectstore's MinIO/S3 client). :nonroot sets a default non-root user,
# though every k8s manifest already pins its own runAsUser: 65534 —
# defense in depth for anywhere this image runs outside that manifest
# (e.g. `docker run` directly).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/app /app
COPY --from=builder /out/grpc-health-probe /usr/local/bin/grpc-health-probe
ENTRYPOINT ["/app"]
