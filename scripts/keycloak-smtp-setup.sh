#!/usr/bin/env bash
# Configure the realm's SMTP server so Keycloak can send invitation emails.
#
# Without this, creating a user fails: POST /api/admin/users sets no password
# and relies entirely on the set-your-password link, so an invitation that
# cannot be sent means an account nobody can reach. The handler treats that as
# a failure and rolls the whole creation back rather than reporting success.
#
# SMTP settings are NOT in the realm import: the password is a credential, and
# realm.json lives in a ConfigMap in git.
#
# Defaults target Resend (smtp.resend.com, username literally "resend", the
# API key as the password). Override SMTP_HOST/SMTP_PORT/SMTP_USER for another
# provider — nothing here is Resend-specific beyond the defaults.
#
#   KEYCLOAK_URL=https://auth.example.com \
#   KEYCLOAK_ADMIN_PASSWORD=... \
#   SMTP_PASSWORD=re_xxx \
#   SMTP_FROM=no-reply@example.com \
#   bash scripts/keycloak-smtp-setup.sh
#
# Verify by sending a real message, not by reading the config back — a wrong
# password is only visible at send time. The script does this for you when
# SMTP_TEST_TO is set.
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:?set KEYCLOAK_URL, e.g. https://auth.example.com}"
REALM="${KEYCLOAK_REALM:-mavericks}"
ADMIN_USER="${KEYCLOAK_ADMIN:-admin}"
ADMIN_PASS="${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD}"

# Exported, not just assigned: the python helpers below read these from the
# environment, and a plain shell assignment is invisible to a child process.
export SMTP_HOST="${SMTP_HOST:-smtp.resend.com}"
export SMTP_PORT="${SMTP_PORT:-587}"
export SMTP_USER="${SMTP_USER:-resend}"
export SMTP_PASSWORD="${SMTP_PASSWORD:?set SMTP_PASSWORD (for Resend, the API key)}"
export SMTP_FROM="${SMTP_FROM:?set SMTP_FROM, e.g. no-reply@example.com}"
# Shown as the sender's name. The realm's displayName (set in the realm import)
# is what the email BODY says — both need to agree, or an invitation arrives
# from one product and talks about another.
export SMTP_FROM_NAME="${SMTP_FROM_NAME:-Maverick}"

log() { echo "  $*" >&2; }

T="$(curl -sf --max-time 30 -X POST \
  "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -d client_id=admin-cli -d grant_type=password \
  --data-urlencode "username=${ADMIN_USER}" \
  --data-urlencode "password=${ADMIN_PASS}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')"
API="${KEYCLOAK_URL}/admin/realms/${REALM}"

# Read-modify-write: a realm PUT takes a whole representation, and sending only
# the SMTP block would blank every other setting on the realm.
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
curl -sf -H "Authorization: Bearer $T" "$API" > "$TMP"

python3 - "$TMP" <<'PY'
import json, os, sys
path = sys.argv[1]
realm = json.load(open(path))
realm["smtpServer"] = {
    "host": os.environ["SMTP_HOST"],
    "port": os.environ["SMTP_PORT"],
    "from": os.environ["SMTP_FROM"],
    "fromDisplayName": os.environ["SMTP_FROM_NAME"],
    "auth": "true",
    "user": os.environ["SMTP_USER"],
    "password": os.environ["SMTP_PASSWORD"],
    # STARTTLS on 587, implicit TLS on 465. Both encrypt; sending an API key
    # over a cleartext session would not.
    "starttls": "true" if os.environ["SMTP_PORT"] == "587" else "false",
    "ssl": "true" if os.environ["SMTP_PORT"] == "465" else "false",
}
json.dump(realm, open(path, "w"))
PY

curl -sf -o /dev/null -X PUT "$API" \
  -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  --data @"$TMP"
log "SMTP configured: ${SMTP_USER}@${SMTP_HOST}:${SMTP_PORT}, from ${SMTP_FROM}"

# Reading the config back proves nothing about whether mail can actually be
# sent; only a real send does.
if [ -n "${SMTP_TEST_TO:-}" ]; then
  log "sending a test message to ${SMTP_TEST_TO}..."
  ADMIN_ID="$(curl -sf -H "Authorization: Bearer $T" \
    "${KEYCLOAK_URL}/admin/realms/master/users?username=${ADMIN_USER}&exact=true" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["id"])')"
  curl -sf -o /dev/null -X PUT \
    "${KEYCLOAK_URL}/admin/realms/master/users/${ADMIN_ID}" \
    -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
    -d "{\"email\":\"${SMTP_TEST_TO}\"}"
  code=$(curl -s -o /tmp/kc-smtp-test.$$ -w '%{http_code}' -X POST \
    "${API}/testSMTPConnection" \
    -H "Authorization: Bearer $T" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "config=$(python3 -c '
import json, os
print(json.dumps({
  "host": os.environ["SMTP_HOST"], "port": os.environ["SMTP_PORT"],
  "from": os.environ["SMTP_FROM"], "fromDisplayName": os.environ["SMTP_FROM_NAME"],
  "auth": "true", "user": os.environ["SMTP_USER"], "password": os.environ["SMTP_PASSWORD"],
  "starttls": "true" if os.environ["SMTP_PORT"] == "587" else "false",
  "ssl": "true" if os.environ["SMTP_PORT"] == "465" else "false",
}))')")
  if [ "$code" = "204" ]; then
    log "test message accepted by ${SMTP_HOST} — check ${SMTP_TEST_TO}"
  else
    log "TEST SEND FAILED (HTTP ${code}): $(head -c 400 /tmp/kc-smtp-test.$$)"
    log "common causes: unverified sending domain, or a wrong API key"
    rm -f /tmp/kc-smtp-test.$$
    exit 1
  fi
  rm -f /tmp/kc-smtp-test.$$
fi
