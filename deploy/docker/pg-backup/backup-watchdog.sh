#!/usr/bin/env bash
# backup-watchdog.sh — answers the only question that matters about a backup
# system: is there a recent, plausible dump in the bucket right now?
#
# It exists because the answer was "no" for three consecutive nights in
# September 2026 and nothing said so. pg-backup.sh failed, the CronJob
# recorded the failure, and the failure sat in `kubectl get jobs` where nobody
# was looking. A backup nobody checks is a belief, not a backup.
#
# Deliberately checks the RESULT rather than the run: it catches a dump that
# failed, a CronJob that was never scheduled, a bucket that was renamed, and
# credentials that stopped working — all of which look identical from here,
# which is the point. Being a separate job from the one it watches is what
# lets it notice the backup not running at all.
#
# Env vars:
#   MINIO_URL               e.g. http://minio:9000 (required)
#   MINIO_ROOT_USER         (required)
#   MINIO_ROOT_PASSWORD     (required)
#   BACKUP_BUCKET           default: mavericks-backups
#   BACKUP_MAX_AGE_HOURS    default: 26. A daily backup plus two hours of
#                           slack, so an ordinary slow night is not an alert
#                           and a missed night always is.
#   BACKUP_MIN_SIZE         default: 1MB. A zero-length or truncated object
#                           is a failure that looks like a success.
#   ALERT_EMAIL             where to report. Unset means nobody is told —
#                           the job still fails loudly, which is all it can do.
#   SMTP_HOST/SMTP_PORT/SMTP_USERNAME/SMTP_PASSWORD/SMTP_FROM
#                           the same relay the gateway sends with.
#
# Exit status: 0 when a recent backup exists, 1 otherwise — so the CronJob
# itself carries the verdict even where the mail does not arrive.

set -euo pipefail

: "${MINIO_URL:?MINIO_URL is required}"
: "${MINIO_ROOT_USER:?MINIO_ROOT_USER is required}"
: "${MINIO_ROOT_PASSWORD:?MINIO_ROOT_PASSWORD is required}"
BACKUP_BUCKET="${BACKUP_BUCKET:-mavericks-backups}"
BACKUP_MAX_AGE_HOURS="${BACKUP_MAX_AGE_HOURS:-26}"
BACKUP_MIN_SIZE="${BACKUP_MIN_SIZE:-1MB}"
ALERT_EMAIL="${ALERT_EMAIL:-}"

log() { echo "[backup-watchdog] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

# alert sends one plain-text mail and never fails the script: a relay that is
# down must not hide the verdict the exit status carries.
alert() {
  local subject="$1"
  local body="$2"
  log "ALERT: ${subject}"
  echo "${body}" | sed 's/^/  /'
  if [ -z "$ALERT_EMAIL" ] || [ -z "${SMTP_HOST:-}" ] || [ -z "${SMTP_FROM:-}" ]; then
    log "no ALERT_EMAIL/SMTP_HOST/SMTP_FROM configured — nobody was told by mail"
    return 0
  fi
  local port="${SMTP_PORT:-587}"
  local scheme="smtp"
  # 465 is implicit TLS; 587 and 25 negotiate it with STARTTLS, which
  # --ssl-reqd makes mandatory rather than optional.
  if [ "$port" = "465" ]; then scheme="smtps"; fi
  local message="/tmp/backup-alert.txt"
  {
    echo "From: ${SMTP_FROM}"
    echo "To: ${ALERT_EMAIL}"
    echo "Subject: ${subject}"
    echo "Date: $(date -uR)"
    echo "Content-Type: text/plain; charset=utf-8"
    echo
    echo "${body}"
  } > "$message"
  if curl --silent --show-error --ssl-reqd --max-time 30 \
      --url "${scheme}://${SMTP_HOST}:${port}" \
      ${SMTP_USERNAME:+--user "${SMTP_USERNAME}:${SMTP_PASSWORD:-}"} \
      --mail-from "${SMTP_FROM}" --mail-rcpt "${ALERT_EMAIL}" \
      --upload-file "$message"; then
    log "alert sent to ${ALERT_EMAIL}"
  else
    log "could not send the alert mail; the job still fails"
  fi
  rm -f "$message"
}

log "checking ${BACKUP_BUCKET}/pg for a backup newer than ${BACKUP_MAX_AGE_HOURS}h and larger than ${BACKUP_MIN_SIZE}"
# Rotated credentials fail here, at `alias set`, before any listing is
# attempted — and `set -e` would end the run silently, which is the exact
# failure this job exists to make noisy.
if ! aliased="$(mc alias set backupminio "$MINIO_URL" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" 2>&1)"; then
  # mc probes the endpoint while setting the alias, and words a connection
  # that never opened as a credentials problem ("Unable to initialize new
  # alias from the provided credentials ... i/o timeout"). Say which it was:
  # a network policy that blocked this job read as rotated keys, and sent the
  # search to the wrong place.
  case "$aliased" in
    *"dial tcp"* | *"i/o timeout"* | *"connection refused"* | *"no such host"*)
      cause="Object storage could not be reached, so whether a backup exists is
unknown."
      hint="Check that the endpoint is right and reachable from this job: a network
policy on either side (this job's egress, the storage's ingress) blocks it
silently."
      ;;
    *)
      cause="Object storage would not accept the credentials, so whether a backup
exists is unknown."
      hint="Check MINIO_ROOT_USER / MINIO_ROOT_PASSWORD in mavericks-secrets."
      ;;
  esac
  alert "[mavericks] backup check could not run" "\
${cause}

  endpoint: ${MINIO_URL}

  ${aliased}

${hint}"
  exit 1
fi

# Two probes before the real question, because the three failures need three
# different answers. Storage that does not answer means the check could not be
# made, which is not the same as a missing backup; a bucket that is gone is a
# missing backup, and saying "check your credentials" about it would send
# someone looking in the wrong place.
if ! reachable="$(mc ls backupminio 2>&1)"; then
  alert "[mavericks] backup check could not run" "\
Object storage did not answer, so whether a backup exists is unknown.

  endpoint: ${MINIO_URL}

  ${reachable}

Check the credentials in mavericks-secrets and that the endpoint is reachable
from the cluster."
  exit 1
fi

if ! mc ls "backupminio/${BACKUP_BUCKET}" >/dev/null 2>&1; then
  alert "[mavericks] the backup bucket does not exist" "\
Object storage is reachable, but there is no bucket ${BACKUP_BUCKET}, so there
are no backups at all.

  endpoint: ${MINIO_URL}
  buckets:  ${reachable:-  (none)}

Either BACKUP_BUCKET names the wrong bucket, or the bucket was removed. The
next successful run of the postgres-backup CronJob creates it."
  exit 1
fi

listing="$(mc find "backupminio/${BACKUP_BUCKET}/pg/" --newer-than "${BACKUP_MAX_AGE_HOURS}h" --larger "${BACKUP_MIN_SIZE}" 2>/dev/null || true)"
count="$(printf '%s' "$listing" | grep -c . || true)"
if [ "$count" -gt 0 ]; then
  log "ok: ${count} backup object(s) within the window"
  printf '%s\n' "$listing" | sed 's/^/  /'
  exit 0
fi

newest="$(mc ls "backupminio/${BACKUP_BUCKET}/pg/" 2>/dev/null | tail -3 || true)"
alert "[mavericks] no database backup in the last ${BACKUP_MAX_AGE_HOURS} hours" "\
No object in ${BACKUP_BUCKET}/pg is newer than ${BACKUP_MAX_AGE_HOURS} hours and
larger than ${BACKUP_MIN_SIZE}. Either the nightly dump failed, or it did not run.

The most recent objects in the bucket:
${newest:-  (the prefix is empty)}

Check the job:
  kubectl -n mavericks get cronjob postgres-backup
  kubectl -n mavericks logs job/\$(kubectl -n mavericks get jobs -l app=postgres-backup \\
    --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')

Run one by hand:
  kubectl -n mavericks create job --from=cronjob/postgres-backup backup-manual-\$(date +%s)"
exit 1
