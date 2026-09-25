#!/usr/bin/env bash
# pg-restore.sh — downloads a backup produced by pg-backup.sh from
# MinIO/S3-compatible storage and restores it into a target database.
#
# This is a deliberately manual, human-triggered operation — never run
# automatically. The target database is DROPPED and created again empty
# before the dump is loaded, so this refuses to run without an explicit
# CONFIRM_RESTORE=yes, on top of requiring RESTORE_DATABASE_URL to be set
# separately from DATABASE_URL so a target is never implicit. Stop whatever
# writes to the database first: its connections are terminated, and a
# writer that reconnects mid-restore would write into a half-loaded database.
#
# Env vars:
#   RESTORE_DATABASE_URL   postgres://user:pass@<target-host>:5432/mavericks (required)
#                          — the server itself, not PgBouncer; the user needs
#                          the right to drop and create databases
#   MINIO_URL              e.g. http://minio:9000 (required)
#   MINIO_ROOT_USER        (required)
#   MINIO_ROOT_PASSWORD    (required)
#   BACKUP_BUCKET          default: mavericks-backups
#   RESTORE_FROM_DB        which database's dumps "latest" chooses from;
#                          default: the database RESTORE_DATABASE_URL names
#   RESTORE_MAINTENANCE_DB the database to connect to while the target is
#                          dropped and created; default: postgres
#   CONFIRM_RESTORE         must be "yes" or this refuses to run
#
# Usage:
#   pg-restore.sh <object-name|latest>
#
# Examples:
#   pg-restore.sh latest
#   pg-restore.sh backup-mavericks-20260811T030000Z.dump
#
# "latest" is the newest dump OF ONE DATABASE. pg-backup.sh writes one object
# per database (backup-<database>-<timestamp>.dump), and the newest object
# overall is usually another database's: names sort by database first, so
# with BACKUP_DATABASES=all it was a tenant_* dump, which was then restored
# over the control-plane database.
#
# Why drop and create rather than pg_restore --clean: --clean into a live
# database cannot drop the primary keys partitions inherit (runtime.fact_input
# and friends), so it reported errors and exited 1 on every real restore,
# leaving the operator to guess whether the data was complete; objects added
# after the backup survived it, too. An empty database takes the dump exactly,
# and --exit-on-error makes any error that remains a real one.

set -euo pipefail

: "${RESTORE_DATABASE_URL:?RESTORE_DATABASE_URL is required}"
: "${MINIO_URL:?MINIO_URL is required}"
: "${MINIO_ROOT_USER:?MINIO_ROOT_USER is required}"
: "${MINIO_ROOT_PASSWORD:?MINIO_ROOT_PASSWORD is required}"
BACKUP_BUCKET="${BACKUP_BUCKET:-mavericks-backups}"
target="${1:?usage: pg-restore.sh <object-name|latest>}"

log() { echo "[pg-restore] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

target_db="$(echo "$RESTORE_DATABASE_URL" | sed -E 's#.*://[^/]+/([^?]*).*#\1#')"
maintenance_url="$(echo "$RESTORE_DATABASE_URL" | sed -E "s#(://[^/]+)/[^?]*#\1/${RESTORE_MAINTENANCE_DB:-postgres}#")"

if [ "${CONFIRM_RESTORE:-}" != "yes" ]; then
  echo "REFUSING TO RUN: this will DROP the database ${target_db} and recreate it from the backup:" >&2
  echo "  RESTORE_DATABASE_URL=${RESTORE_DATABASE_URL}" >&2
  echo "Set CONFIRM_RESTORE=yes to proceed." >&2
  exit 1
fi

log "configuring mc alias"
mc alias set backupminio "$MINIO_URL" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null

if [ "$target" = "latest" ]; then
  source_db="${RESTORE_FROM_DB:-$target_db}"
  log "resolving the latest backup of ${source_db} under pg/"
  # Within one database the timestamp is the only varying part of the name,
  # so a lexical sort is a chronological one.
  object="$(mc find "backupminio/${BACKUP_BUCKET}/pg/" --name "backup-${source_db}-*.dump" | sort | tail -1)"
  if [ -z "$object" ]; then
    echo "no backups of ${source_db} found under backupminio/${BACKUP_BUCKET}/pg/" >&2
    exit 1
  fi
else
  object="backupminio/${BACKUP_BUCKET}/pg/${target}"
fi

dump_file="/tmp/$(basename "$object")"
trap 'rm -f "$dump_file"' EXIT
log "downloading ${object} to ${dump_file}"
mc cp "$object" "$dump_file"

# Read the whole archive's table of contents before anything is dropped: a
# truncated or foreign file fails here, with the database still intact.
log "checking the archive"
pg_restore --list "$dump_file" >/dev/null

log "dropping and recreating ${target_db}"
psql -v ON_ERROR_STOP=1 -q "$maintenance_url" \
  -c "DROP DATABASE IF EXISTS \"${target_db}\" WITH (FORCE)" \
  -c "CREATE DATABASE \"${target_db}\""

log "restoring into the empty database"
pg_restore --exit-on-error --no-owner --no-acl -d "$RESTORE_DATABASE_URL" "$dump_file"

log "restore complete"
