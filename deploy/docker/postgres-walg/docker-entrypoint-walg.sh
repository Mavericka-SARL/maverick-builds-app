#!/usr/bin/env bash
# docker-entrypoint-walg.sh — wraps the official postgres image's own
# entrypoint to ensure the WAL-G target bucket exists before postgres
# (and therefore archive_command) ever starts. wal-g does NOT auto-create
# a missing S3 bucket — confirmed live: wal-g wal-push/backup-push both
# fail outright against a bucket that doesn't exist yet. Idempotent
# (mc mb --ignore-existing), so this is safe to run on every container
# start, not just first-time initialization.
#
# Only runs the bucket-creation step when actually starting postgres
# (this entrypoint's default CMD) — the wal-g-basebackup sidecar
# overrides the container's command directly (basebackup-loop.sh, which
# does its own mc mb before each backup-push, since k8s pods start all
# containers concurrently with no guaranteed ordering between this
# entrypoint and the sidecar).
#
# deploy/k8s/base/infra/postgres.yaml invokes this via `args:` alone
# (no `command:` override), which starts with "-c", not "postgres" — the
# same "-c ...options" shape the official docker-entrypoint.sh itself
# special-cases (it prepends "postgres" when $1 starts with "-", see its
# own source). Match that convention here too, or this check silently
# never fires against the real k8s config.

set -euo pipefail

if [ "${1:-}" = "postgres" ] || [ "${1:-}" = "" ] || [ "${1:0:1}" = "-" ]; then
  if [ -n "${WALG_S3_PREFIX:-}" ]; then
    bucket="$(echo "$WALG_S3_PREFIX" | sed -E 's#^s3://([^/]+).*#\1#')"
    echo "[docker-entrypoint-walg] ensuring bucket ${bucket} exists"
    mc alias set walgminio "$AWS_ENDPOINT" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null
    # Check before creating. `mc mb --ignore-existing` still issues a
    # CreateBucket, and a provider whose region differs from mc's default
    # rejects it on the location constraint BEFORE reaching the
    # already-exists path — so --ignore-existing does not help there. Against
    # Hetzner Object Storage (fsn1) this failed even though the bucket
    # existed, and `set -e` then stopped postgres from starting at all.
    # `mc ls` needs no region and succeeds for an existing (even empty) bucket.
    if ! mc ls "walgminio/${bucket}" >/dev/null 2>&1; then
      mc mb "walgminio/${bucket}" >/dev/null
    fi
  fi
fi

exec docker-entrypoint.sh "$@"
