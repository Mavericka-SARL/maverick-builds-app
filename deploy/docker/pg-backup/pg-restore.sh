#!/usr/bin/env bash
# pg-restore.sh — downloads a backup produced by pg-backup.sh from
# MinIO/S3-compatible storage and restores it into a target database.
#
# This is a deliberately manual, human-triggered operation — never run
# automatically. pg_restore --clean drops existing objects in the target
# database before recreating them, so this refuses to run without an
# explicit CONFIRM_RESTORE=yes, on top of requiring RESTORE_DATABASE_URL
# to be set separately from DATABASE_URL so a target is never implicit.
#
# Env vars:
#   RESTORE_DATABASE_URL   postgres://user:pass@<target-host>:5432/mavericks (required)
#   MINIO_URL              e.g. http://minio:9000 (required)
#   MINIO_ROOT_USER        (required)
#   MINIO_ROOT_PASSWORD    (required)
#   BACKUP_BUCKET          default: mavericks-backups
#   CONFIRM_RESTORE         must be "yes" or this refuses to run
#
# Usage:
#   pg-restore.sh <object-name|latest>
#
# Examples:
#   pg-restore.sh latest
#   pg-restore.sh backup-20260811T030000Z.dump

set -euo pipefail

: "${RESTORE_DATABASE_URL:?RESTORE_DATABASE_URL is required}"
: "${MINIO_URL:?MINIO_URL is required}"
: "${MINIO_ROOT_USER:?MINIO_ROOT_USER is required}"
: "${MINIO_ROOT_PASSWORD:?MINIO_ROOT_PASSWORD is required}"
BACKUP_BUCKET="${BACKUP_BUCKET:-mavericks-backups}"
target="${1:?usage: pg-restore.sh <object-name|latest>}"

log() { echo "[pg-restore] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

if [ "${CONFIRM_RESTORE:-}" != "yes" ]; then
  echo "REFUSING TO RUN: this will DROP AND RECREATE objects in the target database:" >&2
  echo "  RESTORE_DATABASE_URL=${RESTORE_DATABASE_URL}" >&2
  echo "Set CONFIRM_RESTORE=yes to proceed." >&2
  exit 1
fi

log "configuring mc alias"
mc alias set backupminio "$MINIO_URL" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null

if [ "$target" = "latest" ]; then
  log "resolving latest backup under pg/"
  object="$(mc find "backupminio/${BACKUP_BUCKET}/pg/" --name "*.dump" | sort | tail -1)"
  if [ -z "$object" ]; then
    echo "no backups found under backupminio/${BACKUP_BUCKET}/pg/" >&2
    exit 1
  fi
else
  object="backupminio/${BACKUP_BUCKET}/pg/${target}"
fi

dump_file="/tmp/$(basename "$object")"
log "downloading ${object} to ${dump_file}"
mc cp "$object" "$dump_file"

log "restoring into target database (--clean --if-exists)"
pg_restore --clean --if-exists --no-owner --no-acl -d "$RESTORE_DATABASE_URL" "$dump_file"
rm -f "$dump_file"

log "restore complete"
