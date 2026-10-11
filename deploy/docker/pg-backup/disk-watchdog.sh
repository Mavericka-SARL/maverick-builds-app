#!/usr/bin/env bash
# disk-watchdog.sh — says so before the database volume fills or WAL
# archiving falls behind, instead of after Postgres has stopped.
#
# A full volume stops Postgres, and with it every tenant and every sign-in at
# once, not just the tenant that filled it. WAL that cannot be archived piles
# up on the same volume while Postgres goes on accepting writes, so archiving
# failing is silent until the volume is full or a restore is needed. Runs
# beside Postgres with its data volume mounted read-only: the kubelet's
# volume figures are not reachable from inside the cluster without rights
# this should not hold.
#
# Env vars:
#   PGDATA                    the data directory; pg_wal is inside it (required)
#   DISK_WARN_PERCENT         default: 80
#   DISK_CRITICAL_PERCENT     default: 90
#   WAL_MAX_READY             default: 20. WAL segments (16 MB each) waiting
#                             to be archived; a healthy server has 0 or 1.
#   WAL_MAX_READY_MINUTES     default: 60. Age of the oldest waiting segment.
#   CHECK_EVERY_SECONDS       default: 300
#   REMIND_EVERY_HOURS        default: 24. Mail again while a problem lasts.
#   ALERT_WHERE               names the deployment in the subject, e.g. the
#                             namespace; empty for none.
#   ALERT_EMAIL, SMTP_*       as backup-watchdog.sh (alert-mail.sh).
#   ONCE                      "1": one check, then exit 1 if it found a problem.
#
# Mails when a problem starts or gets worse, every REMIND_EVERY_HOURS while it
# lasts, and once when it is over.

set -euo pipefail

: "${PGDATA:?PGDATA is required}"
DISK_WARN_PERCENT="${DISK_WARN_PERCENT:-80}"
DISK_CRITICAL_PERCENT="${DISK_CRITICAL_PERCENT:-90}"
WAL_MAX_READY="${WAL_MAX_READY:-20}"
WAL_MAX_READY_MINUTES="${WAL_MAX_READY_MINUTES:-60}"
CHECK_EVERY_SECONDS="${CHECK_EVERY_SECONDS:-300}"
REMIND_EVERY_HOURS="${REMIND_EVERY_HOURS:-24}"
ONCE="${ONCE:-}"

log() { echo "[disk-watchdog] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

# shellcheck source=alert-mail.sh
. "$(dirname "$0")/alert-mail.sh"

where="${ALERT_WHERE:+ (${ALERT_WHERE})}"

# check sets: disk_level (ok|warning|critical), used_pct, size_h, avail_h,
# wal_ready, wal_oldest_min, wal_behind (yes|no), and problems (a summary,
# empty when all is well).
check() {
  local df_line
  # The filesystem holding PGDATA, whether PGDATA is its mount point or a
  # directory below it.
  df_line="$(df -P -k "$PGDATA" | awk 'NR==2')"
  local size_kb used_kb avail_kb
  size_kb="$(echo "$df_line" | awk '{print $2}')"
  used_kb="$(echo "$df_line" | awk '{print $3}')"
  avail_kb="$(echo "$df_line" | awk '{print $4}')"
  used_pct=$(( used_kb * 100 / (used_kb + avail_kb + 1) ))
  size_h="$(( size_kb / 1048576 )) GiB"
  avail_h="$(( avail_kb / 1048576 )) GiB"
  disk_level=ok
  if [ "$used_pct" -ge "$DISK_CRITICAL_PERCENT" ]; then
    disk_level=critical
  elif [ "$used_pct" -ge "$DISK_WARN_PERCENT" ]; then
    disk_level=warning
  fi

  wal_ready=0
  wal_oldest_min=0
  local now oldest=""
  now="$(date +%s)"
  for f in "$PGDATA"/pg_wal/archive_status/*.ready; do
    [ -e "$f" ] || continue
    wal_ready=$((wal_ready + 1))
    local m
    m="$(stat -c %Y "$f")"
    if [ -z "$oldest" ] || [ "$m" -lt "$oldest" ]; then oldest="$m"; fi
  done
  if [ -n "$oldest" ]; then wal_oldest_min=$(( (now - oldest) / 60 )); fi
  wal_behind=no
  if [ "$wal_ready" -gt "$WAL_MAX_READY" ] || [ "$wal_oldest_min" -gt "$WAL_MAX_READY_MINUTES" ]; then
    wal_behind=yes
  fi

  problems=""
  if [ "$disk_level" != ok ]; then problems="volume ${used_pct}% full"; fi
  if [ "$wal_behind" = yes ]; then
    problems="${problems:+${problems}, }WAL archiving behind"
  fi
}

report() {
  cat <<EOF
Database volume: ${used_pct}% used of ${size_h}, ${avail_h} free (warning at ${DISK_WARN_PERCENT}%, critical at ${DISK_CRITICAL_PERCENT}%).
WAL waiting to be archived: ${wal_ready} segment(s) of 16 MB, the oldest ${wal_oldest_min} minute(s) old (alert above ${WAL_MAX_READY} or ${WAL_MAX_READY_MINUTES} minutes).
Where: ${ALERT_WHERE:-this deployment}, data directory ${PGDATA}.
EOF
}

advice() {
  if [ "$disk_level" != ok ]; then
    cat <<EOF

When the volume is full Postgres stops, and every tenant and sign-in with it.
Grow the volume while it runs (most cloud volumes grow online and cannot
shrink): kubectl -n <namespace> patch pvc postgres-data-postgres-0 \\
  -p '{"spec":{"resources":{"requests":{"storage":"<new size>"}}}}'
then find what grew: SELECT datname, pg_size_pretty(pg_database_size(datname))
FROM pg_database ORDER BY pg_database_size(datname) DESC; and pg_wal's size.
EOF
  fi
  if [ "$wal_behind" = yes ]; then
    cat <<EOF

WAL that cannot be archived stays on this volume until it is full, and
point-in-time recovery has nothing after the last archived segment. Look at
SELECT * FROM pg_stat_archiver; and the postgres container's log for
"archive command failed" (credentials, the bucket, network policy egress).
EOF
  fi
}

last_problems=""
last_mail=0
first=1
while :; do
  check
  now="$(date +%s)"
  if [ -n "$problems" ]; then
    remind=$(( REMIND_EVERY_HOURS * 3600 ))
    if [ "$problems" != "$last_problems" ] || [ $(( now - last_mail )) -ge "$remind" ]; then
      level="warning"
      if [ "$disk_level" = critical ]; then level="CRITICAL"; fi
      subject="[mavericks] database ${level}${where}: ${problems}"
      log "ALERT: ${subject}"
      send_alert_mail "$subject" "$(printf '%s\n%s' "$(report)" "$(advice)")"
      last_mail="$now"
    fi
  elif [ -n "$last_problems" ]; then
    subject="[mavericks] database back to normal${where}"
    log "${subject}"
    send_alert_mail "$subject" "$(report)

It was: ${last_problems}."
  elif [ -n "$first" ] || [ $(( now % 3600 )) -lt "$CHECK_EVERY_SECONDS" ]; then
    # A line at start and about once an hour, so the log shows it is alive.
    log "ok: volume ${used_pct}% used of ${size_h}, ${wal_ready} WAL segment(s) waiting"
  fi
  last_problems="$problems"
  first=""
  if [ -n "$ONCE" ]; then
    [ -z "$problems" ]
    exit
  fi
  sleep "$CHECK_EVERY_SECONDS"
done
