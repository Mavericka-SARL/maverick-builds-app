#!/usr/bin/env bash
# pg-repack.sh — gives back the disk that deleted rows leave in large
# tables, without stopping the workspace (owner's decision I9, 2026-11-10).
#
# The gateway frees the space a delete leaves (a model, a revision, an
# application) with a plain VACUUM, which takes no lock: the space becomes
# free for the table's next rows, but its files keep their size. Rewriting
# a table to its live data used to be VACUUM FULL, which locks the table for
# the whole rewrite — minutes on a table of several gigabytes, while the
# workspace's grids and dashboards wait. pg_repack rewrites it online: it
# copies the table and its indexes while reads and writes go on, and holds
# an exclusive lock only for the moment it swaps the copy in.
#
# Run weekly (Sundays: the CronJob, Compose's backup service). For the
# control-plane database and every tenant_* database on the server:
# creates the pg_repack extension (when the server has it: the
# postgres-walg image), then repacks each table whose files exceed its live
# data (runtime.live_tables(), migration 135) by REPACK_MIN_MB and by
# REPACK_MIN_SHARE of its files. A table pg_repack cannot take (no primary
# key or unique index) is left to the sweep's VACUUM; a table it cannot lock
# within REPACK_WAIT_SECONDS is tried the next night — it never cancels a
# person's query (--no-kill-backend).
#
# Env vars:
#   DATABASE_URL          postgres://user:pass@postgres:5432/mavericks
#                         (required; a superuser, as the backups use)
#   REPACK_MIN_MB         default: 64
#   REPACK_MIN_SHARE      default: 0.2
#   REPACK_WAIT_SECONDS   default: 60
#   REPACK_DATABASES      default: "all" (the control-plane database and
#                         every tenant database); or a comma-separated list
#
# Exits non-zero when a table it chose could not be repacked, after trying
# every other one.

set -uo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
REPACK_MIN_MB="${REPACK_MIN_MB:-64}"
REPACK_MIN_SHARE="${REPACK_MIN_SHARE:-0.2}"
REPACK_WAIT_SECONDS="${REPACK_WAIT_SECONDS:-60}"
REPACK_DATABASES="${REPACK_DATABASES:-all}"

log() { echo "pg-repack: $*"; }

dsn_for() {
  echo "$DATABASE_URL" | sed -E "s#(://[^/]+)/[^?]*#\1/$1#"
}

databases() {
  if [ "$REPACK_DATABASES" != "all" ]; then
    echo "$REPACK_DATABASES" | tr ',' '\n'
    return
  fi
  echo "$DATABASE_URL" | sed -E 's#.*://[^/]+/([^?]*).*#\1#'
  psql -Atqc "SELECT datname FROM pg_database WHERE datname LIKE 'tenant\\_%' AND datallowconn ORDER BY datname" "$DATABASE_URL"
}

# The server, a minute at most: a new pod's network policy can take a few
# seconds to admit it, and the first connections are refused meanwhile —
# the first run on production connected at once and skipped every database
# (2026-11-10).
for _ in $(seq 1 30); do
  psql -Atqc "SELECT 1" "$DATABASE_URL" >/dev/null 2>&1 && break
  sleep 2
done

failed=0
for db in $(databases); do
  dsn="$(dsn_for "$db")"
  if ! available="$(psql -Atqc "SELECT count(*) FROM pg_available_extensions WHERE name = 'pg_repack'" "$dsn" 2>/tmp/pg-repack.out)"; then
    failed=1
    log "${db}: cannot connect: $(tr '\n' ' ' </tmp/pg-repack.out)"
    continue
  fi
  if [ "$available" != "1" ]; then
    log "${db}: the server has no pg_repack (the postgres-walg image carries it); skipped"
    continue
  fi
  if ! psql -Atqc "CREATE EXTENSION IF NOT EXISTS pg_repack" "$dsn" >/dev/null 2>/tmp/pg-repack.out; then
    failed=1
    log "${db}: cannot create the pg_repack extension: $(tr '\n' ' ' </tmp/pg-repack.out)"
    continue
  fi
  if ! tables="$(psql -Atq -v ON_ERROR_STOP=1 -v min_bytes="$((REPACK_MIN_MB * 1048576))" -v share="$REPACK_MIN_SHARE" "$dsn" 2>/tmp/pg-repack.out <<'SQL'
SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) || ' ' || (l.files - l.bytes)::bigint
FROM runtime.live_tables() l
JOIN pg_class c ON c.oid = l.relid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r'
  AND l.files - l.bytes >= :min_bytes
  AND l.files - l.bytes >= :share * l.files
  AND EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisvalid
              AND (i.indisprimary OR (i.indisunique AND i.indimmediate)))
ORDER BY l.files - l.bytes DESC
SQL
)"; then
    failed=1
    log "${db}: cannot list its tables: $(tr '\n' ' ' </tmp/pg-repack.out)"
    continue
  fi
  if [ -z "$tables" ]; then
    continue
  fi
  while read -r table bloat; do
    [ -n "$table" ] || continue
    before="$(psql -Atqc "SELECT pg_total_relation_size('${table}')" "$dsn")"
    if pg_repack --no-kill-backend --wait-timeout="$REPACK_WAIT_SECONDS" -d "$dsn" -t "$table" >/tmp/pg-repack.out 2>&1; then
      after="$(psql -Atqc "SELECT pg_total_relation_size('${table}')" "$dsn")"
      log "${db}: ${table} repacked, $(( (before - after) / 1048576 )) MB given back (${bloat} bytes past its live data)"
    else
      failed=1
      log "${db}: ${table} NOT repacked: $(tr '\n' ' ' </tmp/pg-repack.out)"
    fi
  done <<EOF
$tables
EOF
done
rm -f /tmp/pg-repack.out
exit "$failed"
