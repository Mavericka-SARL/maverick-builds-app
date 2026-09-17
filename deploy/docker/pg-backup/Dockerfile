# postgres-backup — pg_dump/pg_restore against a real postgres:16-alpine
# base (ships pg_dump/pg_restore/psql/bash already) plus the MinIO client
# static binary, for backing up to and restoring from MinIO/S3-compatible
# storage. Deliberately NOT built from the shared deploy/docker/Dockerfile
# (distroless, no shell, no Postgres client tools) — this is Postgres
# tooling, not a Go service.
FROM quay.io/minio/mc:RELEASE.2025-08-13T08-35-41Z AS mc

FROM postgres:16-alpine

COPY --from=mc /usr/bin/mc /usr/local/bin/mc
COPY pg-backup.sh pg-restore.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/pg-backup.sh /usr/local/bin/pg-restore.sh

# uid 70 matches infra/postgres.yaml's own runAsUser/runAsGroup (the
# postgres user baked into this same base image) — not the 65534 +
# read-only-rootfs pattern the Go services use, since pg_dump/pg_restore
# need a writable /tmp for dump files.
USER postgres

ENTRYPOINT ["/usr/local/bin/pg-backup.sh"]
