#!/usr/bin/env bash
# pg-backup.sh — dumps the Mavericks Postgres database and uploads it to
# MinIO/S3-compatible object storage, pruning anything older than the
# configured retention window.
#
# Connects DIRECTLY to postgres:5432, not pgbouncer:5433 — pgbouncer here
# runs in transaction-pooling mode, which is unsafe for pg_dump's need for
# one consistent, session-scoped connection (the same reason pkg/migrate.Run
# holds its advisory lock on one dedicated connection rather than going
# through the pool).
#
# Env vars:
#   DATABASE_URL          postgres://user:pass@postgres:5432/mavericks (required)
#   MINIO_URL              e.g. http://minio:9000 (required)
#   MINIO_ROOT_USER        (required)
#   MINIO_ROOT_PASSWORD    (required)
#   BACKUP_BUCKET          default: mavericks-backups
#   BACKUP_RETENTION_DAYS  default: 14
#   BACKUP_DATABASES       default: the one in DATABASE_URL. "all" also dumps
#                          every tenant_* database (see docs/TENANT_DATABASES.md),
#                          one object per database, so a single tenant can be
#                          restored without touching another.
#
# Usage:
#   pg-backup.sh

set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
: "${MINIO_URL:?MINIO_URL is required}"
: "${MINIO_ROOT_USER:?MINIO_ROOT_USER is required}"
: "${MINIO_ROOT_PASSWORD:?MINIO_ROOT_PASSWORD is required}"
BACKUP_BUCKET="${BACKUP_BUCKET:-mavericks-backups}"
BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"

log() { echo "[pg-backup] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_DATABASES="${BACKUP_DATABASES:-}"

log "configuring mc alias"
mc alias set backupminio "$MINIO_URL" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null

log "ensuring bucket ${BACKUP_BUCKET} exists"
mc mb --ignore-existing "backupminio/${BACKUP_BUCKET}" >/dev/null

# The DSN with its database name replaced, so every database on the same
# server is reached with the same credentials.
dsn_for() {
  echo "$DATABASE_URL" | sed -E "s#(://[^/]+)/[^?]*#\1/$1#"
}

dump_one() {
  local db="$1" file="/tmp/backup-${db}-${timestamp}.dump"
  log "dumping ${db} (custom format)"
  pg_dump -Fc --no-owner --no-acl "$(dsn_for "$db")" -f "$file"
  log "uploading $(basename "$file")"
  mc cp "$file" "backupminio/${BACKUP_BUCKET}/pg/$(basename "$file")"
  rm -f "$file"
}

control_db="$(echo "$DATABASE_URL" | sed -E 's#.*://[^/]+/([^?]*).*#\1#')"
dump_one "$control_db"

if [ "$BACKUP_DATABASES" = "all" ]; then
  # Every tenant database, listed from the server itself so a tenant created
  # since the last run is included without configuration.
  tenants="$(psql -Atqc "SELECT datname FROM pg_database WHERE datname LIKE 'tenant\\_%' AND datallowconn ORDER BY datname" "$DATABASE_URL")"
  count="$(echo "$tenants" | grep -c . || true)"
  log "dumping ${count} tenant database(s)"
  for db in $tenants; do
    dump_one "$db"
  done
fi

log "pruning backups older than ${BACKUP_RETENTION_DAYS} days"
mc find "backupminio/${BACKUP_BUCKET}/pg/" --older-than "${BACKUP_RETENTION_DAYS}d" --exec "mc rm {}" || true

log "backup complete"
