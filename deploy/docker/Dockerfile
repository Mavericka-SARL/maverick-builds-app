# Generic multi-stage build for every Go service under cmd/ — build with
# --build-arg SERVICE=<name> matching a cmd/<name> directory, e.g.:
#   docker build --build-arg SERVICE=gateway -f deploy/docker/Dockerfile -t gateway .
#
# One Dockerfile for both Go programs (gateway and integration; see Makefile's
# SERVICES list) — they share the same build (CGO_ENABLED=0, static) and
# runtime shape (deploy/k8s/base/services/*.yaml run them as UID 65534,
# read-only root filesystem, no shell). Both answer HTTP health probes, so
# the image needs no probe binary.
ARG GO_VERSION=1.26-bookworm
FROM golang:${GO_VERSION} AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG SERVICE
RUN test -d "./cmd/${SERVICE}" || (echo "no such service: cmd/${SERVICE}" >&2 && exit 1)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app "./cmd/${SERVICE}"

# The notices every shipped binary must carry: Go's licence and every module
# linked into this service, read from the module cache the build above
# filled. It fails on a dependency whose licence is missing or unreviewed
# (cmd/third-party-notices; docs/LICENSING.md).
RUN go run ./cmd/third-party-notices -out /out/THIRD_PARTY_NOTICES.txt "./cmd/${SERVICE}"

# static-debian12 (not base-debian12): every service binary above is fully
# static (CGO_ENABLED=0), and static-debian12 still carries ca-certificates
# + tzdata, needed for outbound TLS calls (e.g. the gateway's LLM providers,
# objectstore's MinIO/S3 client). :nonroot sets a default non-root user,
# though every k8s manifest already pins its own runAsUser: 65534 —
# defense in depth for anywhere this image runs outside that manifest
# (e.g. `docker run` directly).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/app /app
# Mavericka's licences (Community, and the Enterprise Features licence for the ee/
# code compiled in) and the third-party notices, at /licenses/.
COPY LICENSE /licenses/LICENSE
COPY ee/LICENSE /licenses/ee-LICENSE
COPY --from=builder /out/THIRD_PARTY_NOTICES.txt /licenses/THIRD_PARTY_NOTICES.txt
ENTRYPOINT ["/app"]
