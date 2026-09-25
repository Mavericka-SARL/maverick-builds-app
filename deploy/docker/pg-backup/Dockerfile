# postgres-backup — pg_dump/pg_restore against a real postgres:16-alpine
# base (ships pg_dump/pg_restore/psql/bash already) plus the MinIO client
# static binary, for backing up to and restoring from MinIO/S3-compatible
# storage. Deliberately NOT built from the shared deploy/docker/Dockerfile
# (distroless, no shell, no Postgres client tools) — this is Postgres
# tooling, not a Go service.
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

COPY --from=mc /usr/bin/mc /usr/local/bin/mc
COPY pg-backup.sh pg-restore.sh backup-watchdog.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/pg-backup.sh /usr/local/bin/pg-restore.sh \
    /usr/local/bin/backup-watchdog.sh \
    # curl is the watchdog's SMTP client: it is the one thing in this image
    # that has to reach a mail relay, and adding a Go binary or a Python
    # runtime for six lines of message would be worse.
    && apk add --no-cache curl

# uid 70 matches infra/postgres.yaml's own runAsUser/runAsGroup (the
# postgres user baked into this same base image) — not the 65534 +
# read-only-rootfs pattern the Go services use, since pg_dump/pg_restore
# need a writable /tmp for dump files.
USER postgres

# The default is the nightly dump; the watchdog CronJob overrides the command
# with backup-watchdog.sh (deploy/k8s/base/infra/postgres-backup.yaml).
ENTRYPOINT ["/usr/local/bin/pg-backup.sh"]
