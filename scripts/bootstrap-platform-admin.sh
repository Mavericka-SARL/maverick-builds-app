#!/usr/bin/env bash
# Create the first platform administrator of a deployment.
#
# A fresh deployment has a realm, a database and nobody who can sign in: the
# console creates users, but only for someone already signed in as an
# administrator. This makes that first person — a Keycloak account with a
# password and the platform_admin realm role, and the matching identity.user
# row with the platform_admin role assignment — so everyone after them is
# created through the console. Idempotent: an existing account is reused.
#
#   KEYCLOAK_URL=https://auth.example.com \
#   KEYCLOAK_ADMIN_PASSWORD=... \
#   ADMIN_EMAIL=ops@example.com ADMIN_PASSWORD=... ADMIN_NAME="Ops Admin" \
#   PSQL="kubectl -n mavericks exec -i postgres-0 -c postgres -- psql -U mavericks -d mavericks" \
#   bash scripts/bootstrap-platform-admin.sh
#
# PSQL is any command that reads SQL on stdin against the control-plane
# database (the one DATABASE_URL points at); `psql "$DATABASE_URL"` works
# where psql can reach it. The password is set as non-temporary, so the
# person signs in with it directly; change it in the account console after.
#
# The same mechanics make any known-password account, which staging needs
# for cmd/loadtest: ROLES (comma-separated realm/application roles, default
# platform_admin) and CUSTOMER_ID (the tenant the account belongs to, for
# tenant_admin/developer accounts) — e.g. ROLES=tenant_admin,developer
# CUSTOMER_ID=<tenant uuid> for an account that can seed and drive a model.
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:?set KEYCLOAK_URL}"
REALM="${KEYCLOAK_REALM:-mavericks}"
ADMIN_USER="${KEYCLOAK_ADMIN:-admin}"
ADMIN_PASS="${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD}"
EMAIL="$(echo "${ADMIN_EMAIL:?set ADMIN_EMAIL}" | tr '[:upper:]' '[:lower:]')"
PASSWORD="${ADMIN_PASSWORD:?set ADMIN_PASSWORD}"
NAME="${ADMIN_NAME:-Platform Administrator}"
PSQL="${PSQL:?set PSQL to a command that runs SQL from stdin against the control-plane database}"
ROLES="${ROLES:-platform_admin}"
CUSTOMER_ID="${CUSTOMER_ID:-}"

log() { echo "  $*" >&2; }

# CURL_OPTS lets the script reach a deployment whose DNS or certificate does
# not exist yet, e.g. CURL_OPTS="--resolve auth.example.com:443:203.0.113.10 -k".
curl() { command curl ${CURL_OPTS:-} "$@"; }

T="$(curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -d client_id=admin-cli -d grant_type=password \
  --data-urlencode "username=${ADMIN_USER}" --data-urlencode "password=${ADMIN_PASS}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')"
API="${KEYCLOAK_URL}/admin/realms/${REALM}"

FIRST="${NAME%% *}"; LAST="${NAME#* }"; [ "$LAST" = "$NAME" ] && LAST="Admin"

SUB="$(curl -sf -H "Authorization: Bearer $T" "${API}/users?exact=true&email=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1]))' "$EMAIL")" \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d[0]["id"] if d else "")')"
if [ -z "$SUB" ]; then
  log "creating identity-provider account ${EMAIL}"
  curl -sf -o /dev/null -X POST "${API}/users" -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
    -d "$(python3 -c 'import sys,json; print(json.dumps({"username": sys.argv[1], "email": sys.argv[1], "firstName": sys.argv[2], "lastName": sys.argv[3], "enabled": True, "emailVerified": True}))' "$EMAIL" "$FIRST" "$LAST")"
  SUB="$(curl -sf -H "Authorization: Bearer $T" "${API}/users?exact=true&email=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1]))' "$EMAIL")" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["id"])')"
else
  log "identity-provider account ${EMAIL} already exists"
fi

log "setting the password"
curl -sf -o /dev/null -X PUT "${API}/users/${SUB}/reset-password" -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  -d "$(python3 -c 'import sys,json; print(json.dumps({"type": "password", "value": sys.argv[1], "temporary": False}))' "$PASSWORD")"

ROLE_SQL=""
for role in $(echo "$ROLES" | tr ',' ' '); do
  ROLE_JSON="$(curl -sf -H "Authorization: Bearer $T" "${API}/roles/${role}")"
  log "granting the ${role} realm role"
  curl -sf -o /dev/null -X POST "${API}/users/${SUB}/role-mappings/realm" -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
    -d "[$(echo "$ROLE_JSON" | python3 -c 'import sys,json; r=json.load(sys.stdin); print(json.dumps({"id": r["id"], "name": r["name"]}))')]"
  ROLE_SQL="${ROLE_SQL}INSERT INTO identity.role_assignment (user_id, role) SELECT id, '${role}' FROM identity.\"user\" WHERE keycloak_sub = '${SUB}' ON CONFLICT DO NOTHING;
"
done

CUSTOMER_SQL="NULL"
[ -n "$CUSTOMER_ID" ] && CUSTOMER_SQL="'${CUSTOMER_ID}'::uuid"
log "writing the application user row"
$PSQL <<SQL
INSERT INTO identity."user" (keycloak_sub, email, display_name, customer_id)
VALUES ('${SUB}', '${EMAIL}', '$(echo "$NAME" | sed "s/'/''/g")', ${CUSTOMER_SQL})
ON CONFLICT (keycloak_sub) DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name,
  customer_id = COALESCE(EXCLUDED.customer_id, identity."user".customer_id);
${ROLE_SQL}
SQL

log "done: ${EMAIL} holds ${ROLES} (subject ${SUB})"
