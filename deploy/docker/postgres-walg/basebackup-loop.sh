#!/usr/bin/env bash
# basebackup-loop.sh — sidecar entrypoint: periodically pushes a WAL-G
# base backup (the starting point WAL replay needs for point-in-time
# recovery — continuous wal-g wal-push archiving alone isn't enough)
# and prunes old ones. Runs in the same pod as postgres, sharing its
# PGDATA volume read-only.
#
# Env vars: standard WAL-G S3 vars (AWS_ENDPOINT, AWS_ACCESS_KEY_ID,
# AWS_SECRET_ACCESS_KEY, AWS_S3_FORCE_PATH_STYLE, AWS_REGION,
# WALG_S3_PREFIX) plus:
#   PGDATA                     (required)
#   PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE
#                              (required — wal-g backup-push calls
#                              pg_start_backup()/pg_stop_backup() over a
#                              REGULAR SQL connection, not the replication
#                              protocol, confirmed live; this runs in a
#                              separate container from postgres's own, so
#                              it needs real libpq connection env vars,
#                              not OS-user trust — confirmed live that
#                              without these it defaults to OS user "root"
#                              and fails)
#   WALG_BACKUP_RETAIN_COUNT   default: 3
#   WALG_BACKUP_INTERVAL_SECS  default: 86400

set -euo pipefail

: "${PGDATA:?PGDATA is required}"
: "${PGHOST:?PGHOST is required}"
: "${PGUSER:?PGUSER is required}"
: "${PGPASSWORD:?PGPASSWORD is required}"
: "${PGDATABASE:?PGDATABASE is required}"
: "${WALG_S3_PREFIX:?WALG_S3_PREFIX is required}"
WALG_BACKUP_RETAIN_COUNT="${WALG_BACKUP_RETAIN_COUNT:-3}"
WALG_BACKUP_INTERVAL_SECS="${WALG_BACKUP_INTERVAL_SECS:-86400}"

log() { echo "[basebackup] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

# wal-g does not auto-create a missing bucket (confirmed live) — this
# sidecar starts concurrently with, not after, the postgres container in
# a k8s pod, so it can't rely on that container's own entrypoint having
# created it first. Idempotent, cheap, safe to repeat every iteration.
ensure_bucket() {
  local bucket
  bucket="$(echo "$WALG_S3_PREFIX" | sed -E 's#^s3://([^/]+).*#\1#')"
  mc alias set walgminio "$AWS_ENDPOINT" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null
  # See docker-entrypoint-walg.sh: --ignore-existing does not survive a
  # region/location-constraint mismatch, so check before creating.
  if ! mc ls "walgminio/${bucket}" >/dev/null 2>&1; then
    mc mb "walgminio/${bucket}" >/dev/null
  fi
}

while true; do
  ensure_bucket
  log "pushing base backup of ${PGDATA}"
  if wal-g backup-push "$PGDATA"; then
    log "base backup complete"
    log "pruning, retaining last ${WALG_BACKUP_RETAIN_COUNT}"
    wal-g delete retain "$WALG_BACKUP_RETAIN_COUNT" --confirm || log "prune failed (non-fatal)"
  else
    log "base backup FAILED, will retry next interval"
  fi
  sleep "$WALG_BACKUP_INTERVAL_SECS"
done
