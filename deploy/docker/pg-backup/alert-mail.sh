#!/usr/bin/env bash
# alert-mail.sh — sourced by the watchdogs in this image (backup-watchdog.sh,
# disk-watchdog.sh), so an alert reaches people one way only.
#
# send_alert_mail SUBJECT BODY sends one plain-text mail to ALERT_EMAIL
# through the relay the gateway sends with (SMTP_HOST, SMTP_PORT,
# SMTP_USERNAME, SMTP_PASSWORD, SMTP_FROM). It never fails the caller: a
# relay that is down must not hide the verdict the caller reports. It logs
# through the caller's log function.

send_alert_mail() {
  local subject="$1"
  local body="$2"
  if [ -z "${ALERT_EMAIL:-}" ] || [ -z "${SMTP_HOST:-}" ] || [ -z "${SMTP_FROM:-}" ]; then
    log "no ALERT_EMAIL/SMTP_HOST/SMTP_FROM configured — nobody was told by mail"
    return 0
  fi
  local port="${SMTP_PORT:-587}"
  local scheme="smtp"
  # 465 is implicit TLS; 587 and 25 negotiate it with STARTTLS, which
  # --ssl-reqd makes mandatory rather than optional.
  if [ "$port" = "465" ]; then scheme="smtps"; fi
  local message
  message="$(mktemp /tmp/alert.XXXXXX)"
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
    log "could not send the alert mail"
  fi
  rm -f "$message"
}
