#!/bin/sh
# Daily database dumps for the Compose stack: runs pg-backup.sh (the script
# the Kubernetes CronJob runs) once a day at BACKUP_HOUR, UTC.
#
#   backup-schedule.sh        wait for BACKUP_HOUR, back up, repeat
#   backup-schedule.sh now    back up once, immediately, and exit
#
# A failed run is logged and retried the next day rather than stopping the
# container: a backup loop that dies quietly is the failure that matters.
# With ALERT_EMAIL set, every run is followed by backup-watchdog.sh (the
# check the Kubernetes CronJob runs each morning), asking whether this run
# left a plausible dump in the bucket — and e-mailing when it did not.
# docs/SELF_HOSTING.md shows how to check that dumps are actually arriving.
set -u

if [ "${1:-}" = "now" ]; then
  exec pg-backup.sh
fi

hour="${BACKUP_HOUR:-3}"
echo "[backup-schedule] daily dump at ${hour}:00 UTC (and the repack on Sundays)${ALERT_EMAIL:+, alerts to ${ALERT_EMAIL}}"
while :; do
  now="$(date -u +%s)"
  wait=$(( (hour * 3600 - now % 86400 + 86400) % 86400 ))
  [ "$wait" -eq 0 ] && wait=86400
  sleep "$wait"
  pg-backup.sh || echo "[backup-schedule] $(date -u +%Y-%m-%dT%H:%M:%SZ) backup FAILED; next attempt in 24 hours" >&2
  # On Sundays, then give back the disk deletes left in large tables,
  # online (pg-repack.sh; the server needs the postgres-walg image).
  if [ "$(date -u +%u)" = 7 ]; then
    pg-repack.sh || echo "[backup-schedule] $(date -u +%Y-%m-%dT%H:%M:%SZ) repack left a table as it was; see the lines above" >&2
  fi
  if [ -n "${ALERT_EMAIL:-}" ]; then
    # Two hours: this run's objects, not yesterday's.
    BACKUP_MAX_AGE_HOURS=2 backup-watchdog.sh || true
  fi
done
