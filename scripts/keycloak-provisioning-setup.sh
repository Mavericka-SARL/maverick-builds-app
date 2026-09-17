#!/usr/bin/env bash
# Finish setting up the gateway's user-provisioning service account.
#
# The realm import declares the mavericks-admin client, but two things cannot
# come from a ConfigMap:
#
#   1. Its realm-management role grants. Keycloak creates the realm-management
#      client itself, so the role IDs do not exist until the realm does.
#   2. Its secret, which Keycloak generates. Putting a secret in git is exactly
#      what sealed-secrets exists to avoid.
#
# Run once per cluster, after the realm exists. Idempotent.
#
#   KEYCLOAK_URL=https://auth.example.com \
#   KEYCLOAK_ADMIN_PASSWORD=... \
#   bash scripts/keycloak-provisioning-setup.sh
#
# Prints the client secret on stdout and nothing else, so it can be captured:
#   SECRET=$(bash scripts/keycloak-provisioning-setup.sh)
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:?set KEYCLOAK_URL, e.g. https://auth.example.com}"
REALM="${KEYCLOAK_REALM:-mavericks}"
ADMIN_USER="${KEYCLOAK_ADMIN:-admin}"
ADMIN_PASS="${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD}"
CLIENT_ID="${KEYCLOAK_ADMIN_CLIENT_ID:-mavericks-admin}"

log() { echo "  $*" >&2; }

# CURL_OPTS lets the script reach a deployment whose DNS or certificate does
# not exist yet, e.g. CURL_OPTS="--resolve auth.example.com:443:203.0.113.10 -k".
curl() { command curl ${CURL_OPTS:-} "$@"; }

token() {
  curl -sf --max-time 30 -X POST \
    "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
    -d client_id=admin-cli -d grant_type=password \
    --data-urlencode "username=${ADMIN_USER}" \
    --data-urlencode "password=${ADMIN_PASS}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])'
}

T="$(token)"
API="${KEYCLOAK_URL}/admin/realms/${REALM}"

# ── the client itself ─────────────────────────────────────────────────────
UUID="$(curl -sf -H "Authorization: Bearer $T" "${API}/clients?clientId=${CLIENT_ID}" \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d[0]["id"] if d else "")')"

if [ -z "$UUID" ]; then
  log "creating client ${CLIENT_ID}"
  curl -sf -o /dev/null -X POST "${API}/clients" \
    -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
    -d "{\"clientId\":\"${CLIENT_ID}\",\"name\":\"Gateway user provisioning\",
         \"enabled\":true,\"protocol\":\"openid-connect\",\"publicClient\":false,
         \"standardFlowEnabled\":false,\"implicitFlowEnabled\":false,
         \"directAccessGrantsEnabled\":false,\"serviceAccountsEnabled\":true}"
  UUID="$(curl -sf -H "Authorization: Bearer $T" "${API}/clients?clientId=${CLIENT_ID}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["id"])')"
else
  log "client ${CLIENT_ID} already exists"
fi

# ── grant only what provisioning needs ────────────────────────────────────
# manage-users covers create/delete and execute-actions-email; view-users is
# needed for the lookup that makes creation idempotent; view-realm is needed to
# READ a realm role before assigning it, since the assignment API takes the
# role's id, not its name. Without view-realm, creating a user fails at
# "look up realm role" with a bare 403 — user management appears broken while
# the credential looks correct.
#
# Deliberately NOT realm-admin: this credential lives in the gateway, and the
# blast radius of it leaking should stop at user administration for one realm.
# view-realm is read-only realm configuration, not the ability to change it.
SA_ID="$(curl -sf -H "Authorization: Bearer $T" "${API}/clients/${UUID}/service-account-user" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')"
RM_ID="$(curl -sf -H "Authorization: Bearer $T" "${API}/clients?clientId=realm-management" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["id"])')"

ROLES_JSON="$(curl -sf -H "Authorization: Bearer $T" "${API}/clients/${RM_ID}/roles" \
  | python3 -c '
import sys, json
want = {"manage-users", "view-users", "view-realm", "view-identity-providers", "manage-identity-providers"}
print(json.dumps([{"id": r["id"], "name": r["name"]} for r in json.load(sys.stdin) if r["name"] in want]))')"

log "granting manage-users, view-users and view-realm to the service account"
curl -sf -o /dev/null -X POST "${API}/users/${SA_ID}/role-mappings/clients/${RM_ID}" \
  -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  -d "$ROLES_JSON"

GRANTED="$(curl -sf -H "Authorization: Bearer $T" "${API}/users/${SA_ID}/role-mappings/clients/${RM_ID}" \
  | python3 -c 'import sys,json; print(",".join(sorted(r["name"] for r in json.load(sys.stdin))))')"
log "service account now holds: ${GRANTED}"

# ── the secret ────────────────────────────────────────────────────────────
SECRET="$(curl -sf -H "Authorization: Bearer $T" "${API}/clients/${UUID}/client-secret" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["value"])')"

log ""
log "Add to the sealed secret as KEYCLOAK_ADMIN_CLIENT_SECRET, and set"
log "KEYCLOAK_ADMIN_CLIENT_ID=${CLIENT_ID} in mavericks-config."
printf '%s\n' "$SECRET"
