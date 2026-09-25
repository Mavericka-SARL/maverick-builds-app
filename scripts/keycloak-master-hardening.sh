#!/usr/bin/env bash
# Turn on brute-force protection for Keycloak's master realm.
#
# The platform's own realm gets it from its realm import (bruteForceProtected
# in deploy/k8s/base/infra/keycloak.yaml and the Compose realm). The master
# realm — the one Keycloak's administrator signs in to, at /admin — is created
# by Keycloak itself, so nothing in this repository describes it, and it
# starts with protection OFF while being just as reachable. Run once per
# deployment. Idempotent.
#
#   KEYCLOAK_URL=https://auth.example.com \
#   KEYCLOAK_ADMIN_PASSWORD=... \
#   bash scripts/keycloak-master-hardening.sh
#
# permanentLockout stays false: a permanent lock lets anyone who knows the
# administrator's user name lock them out for good by guessing wrong.
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:?set KEYCLOAK_URL, e.g. https://auth.example.com}"
ADMIN_USER="${KEYCLOAK_ADMIN:-admin}"
ADMIN_PASS="${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD}"

log() { echo "  $*" >&2; }

# CURL_OPTS lets the script reach a deployment whose DNS or certificate does
# not exist yet, e.g. CURL_OPTS="--resolve auth.example.com:443:203.0.113.10 -k"
# — the same switch the other keycloak-*.sh scripts take.
curl() { command curl ${CURL_OPTS:-} "$@"; }

T="$(curl -sf --max-time 30 -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -d client_id=admin-cli -d grant_type=password \
  --data-urlencode "username=${ADMIN_USER}" --data-urlencode "password=${ADMIN_PASS}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')"

curl -sf -o /dev/null -X PUT "${KEYCLOAK_URL}/admin/realms/master" \
  -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  -d '{"bruteForceProtected":true,"failureFactor":10,"quickLoginCheckMilliSeconds":1000,"permanentLockout":false}'

STATE="$(curl -sf -H "Authorization: Bearer $T" "${KEYCLOAK_URL}/admin/realms/master" \
  | python3 -c 'import sys,json; r=json.load(sys.stdin); print(r["bruteForceProtected"], r["failureFactor"], r["permanentLockout"])')"
[ "${STATE%% *}" = "True" ] || { echo "the master realm still reports: ${STATE}" >&2; exit 1; }
log "master realm: bruteForceProtected, failureFactor, permanentLockout = ${STATE}"
