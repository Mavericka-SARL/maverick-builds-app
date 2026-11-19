#!/usr/bin/env bash
# Point the console's sign-in client at the console's current address.
#
# Keycloak reads its realm file only on first start, so the `mavericks-web`
# client keeps the console address the deployment first started with. After
# CONSOLE_HOST changes (or a first start with the example names), Keycloak
# refuses the console: the sign-in page says "Invalid parameter:
# redirect_uri", or the console cannot start sign-in at all. This sets the
# client's three addresses from CONSOLE_HOST and says what it changed. The
# old addresses are replaced, not kept: an address the deployment no longer
# owns must not stay a place Keycloak sends people back to.
#
#   KEYCLOAK_URL=https://auth.example.com KEYCLOAK_ADMIN_PASSWORD=... \
#   CONSOLE_HOST=mavericks.example.com bash scripts/keycloak-console-client.sh
#
# Safe to repeat. ./setup.sh keycloak runs it on the Compose stack.
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:?set KEYCLOAK_URL, e.g. https://auth.example.com}"
ADMIN_USER="${KEYCLOAK_ADMIN:-admin}"
ADMIN_PASS="${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD}"
CONSOLE_HOST="${CONSOLE_HOST:?set CONSOLE_HOST, e.g. mavericks.example.com (no https://)}"
REALM="${KEYCLOAK_REALM:-mavericks}"
CLIENT_ID="${KEYCLOAK_CONSOLE_CLIENT:-mavericks-web}"

case "$CONSOLE_HOST" in
  *://*|*/*) echo "error: CONSOLE_HOST is a host name, without https:// or a path: $CONSOLE_HOST" >&2; exit 1 ;;
esac

log() { echo "  $*" >&2; }

curl() { command curl ${CURL_OPTS:-} "$@"; }

T="$(curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -d client_id=admin-cli -d grant_type=password \
  --data-urlencode "username=${ADMIN_USER}" --data-urlencode "password=${ADMIN_PASS}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')"

CLIENTS="$(curl -sf --max-time 30 -H "Authorization: Bearer $T" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients?clientId=${CLIENT_ID}")"

# Prints the client to PUT back (or nothing when it already matches), and on
# stderr what changes.
UPDATED="$(printf '%s' "$CLIENTS" | CONSOLE_HOST="$CONSOLE_HOST" CLIENT_ID="$CLIENT_ID" python3 -c '
import json, os, sys
clients = json.load(sys.stdin)
if not clients:
    sys.exit("error: realm has no client " + os.environ["CLIENT_ID"])
c = clients[0]
origin = "https://" + os.environ["CONSOLE_HOST"]
want = {"redirectUris": [origin + "/*"], "webOrigins": [origin]}
attrs = c.setdefault("attributes", {})
changed = []
for key, value in want.items():
    if c.get(key) != value:
        changed.append("%s: %s -> %s" % (key, c.get(key), value))
        c[key] = value
if attrs.get("post.logout.redirect.uris") != origin + "/*":
    changed.append("post logout: %s -> %s" % (attrs.get("post.logout.redirect.uris"), origin + "/*"))
    attrs["post.logout.redirect.uris"] = origin + "/*"
for line in changed:
    print("  " + line, file=sys.stderr)
if changed:
    print(json.dumps(c))
')"

if [ -z "$UPDATED" ]; then
  log "${CLIENT_ID} already allows https://${CONSOLE_HOST}"
  exit 0
fi

ID="$(printf '%s' "$UPDATED" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')"
printf '%s' "$UPDATED" | curl -sf --max-time 30 -o /dev/null -X PUT \
  -H "Authorization: Bearer $T" -H "Content-Type: application/json" --data-binary @- \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/clients/${ID}"
log "${CLIENT_ID} now allows https://${CONSOLE_HOST}"
