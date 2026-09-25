# postgres-walg — postgres:16-alpine plus the wal-g binary, built from
# source (wal-g has no first-party Docker image, only unofficial
# community ones — building from a pinned git tag via the Go toolchain
# matches how this repo already trusts Go module sources over
# third-party binaries everywhere else). This becomes the actual
# database server's own image (archive_command/restore_command need the
# binary local to the postgres process) as well as the base-backup
# sidecar's image — deliberately minimal, no unrelated tooling, unlike
# deploy/docker/pg-backup/ (logical pg_dump/pg_restore backups, a
# separate, complementary mechanism).
FROM golang:1.25 AS walg-builder
ARG WALG_VERSION=v3.0.8
RUN git clone --depth 1 --branch ${WALG_VERSION} https://github.com/wal-g/wal-g.git /src
WORKDIR /src/main/pg
# wal-g's own Makefile sets this (export GOEXPERIMENT=jsonv2) — without
# it, cmd/common/internal's use of the experimental encoding/json/v2
# stdlib package fails to build ("build constraints exclude all Go
# files").
ENV GOEXPERIMENT=jsonv2
# CGO_ENABLED=0 (matches deploy/docker/Dockerfile's own convention): the
# builder stage is glibc-based (golang:1.25) but the final image is
# musl-based (postgres:16-alpine) — a CGO-linked binary built here would
# fail with a misleading "no such file or directory" at runtime (missing
# glibc dynamic loader, not actually a missing file).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /wal-g .

# mc, built from source rather than copied out of MinIO's published image.
# quay.io/minio/mc stopped answering anonymous pulls on 2026-09-24 — 401 on
# every tag, the server image with it — which is the second time MinIO has
# withdrawn a distribution channel under this repo (Docker Hub's copy
# vanished on 2026-09-11, which is why this said quay.io at all). The source
# is still public and MC_VERSION below is the same tag the published image
# carried, so the binary is the one these scripts were verified against.
# CGO_ENABLED=0 for the same reason as every other builder stage here: this
# is glibc, the final image is musl.
FROM golang:1.25 AS mc
ARG MC_VERSION=RELEASE.2025-08-13T08-35-41Z
RUN git clone --depth 1 --branch ${MC_VERSION} https://github.com/minio/mc.git /src
WORKDIR /src
# Stamp the version variables mc's own Makefile stamps, so `mc --version` in a
# running container names the release instead of "DEVELOPMENT.GOGET". These
# scripts have been caught out by a behaviour change between mc versions
# before (see pg-backup.sh on the bucket-region warning), so being able to ask
# the image which one it carries is worth two ldflags. -tags kqueue is what
# upstream's own release build uses.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -tags kqueue \
      -ldflags="-s -w -X github.com/minio/mc/cmd.ReleaseTag=${MC_VERSION} -X github.com/minio/mc/cmd.CommitID=$(git rev-parse HEAD)" \
      -o /usr/bin/mc .

FROM postgres:16-alpine

COPY --from=walg-builder /wal-g /usr/local/bin/wal-g
COPY --from=mc /usr/bin/mc /usr/local/bin/mc
COPY basebackup-loop.sh docker-entrypoint-walg.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/basebackup-loop.sh /usr/local/bin/docker-entrypoint-walg.sh

ENTRYPOINT ["docker-entrypoint-walg.sh"]
CMD ["postgres"]
