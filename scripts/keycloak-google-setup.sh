#!/usr/bin/env bash
# Offer "Sign in with Google" on the realm's sign-in page, and "Sign up with
# Google" on the console's sign-up page.
#
# Keycloak brokers Google as the identity provider "google". Its first-login
# flow (made here: "mavericks google first login") links a Google account to
# the realm account with the same address — Google has verified the address —
# or makes a new realm account. The gateway signs a new one up as the sign-up
# form does (a tenant on the self-service edition: google_signin.go), where
# self-service sign-up is open, and refuses it with the reason where it is
# not. The console finds the provider here; nothing else needs configuring.
#
# Create the OAuth client first, at Google Cloud Console › APIs & Services ›
# Credentials › Create credentials › OAuth client ID, type "Web application",
# with this authorised redirect URI:
#
#   https://auth.<domain>/realms/mavericks/broker/google/endpoint
#
# then:
#
#   KEYCLOAK_URL=https://auth.example.com \
#   KEYCLOAK_ADMIN_PASSWORD=... \
#   GOOGLE_CLIENT_ID=...apps.googleusercontent.com \
#   GOOGLE_CLIENT_SECRET=... \
#   bash scripts/keycloak-google-setup.sh
#
# Idempotent. GOOGLE_ENABLED=false keeps the provider and takes the buttons
# away. Without GOOGLE_CLIENT_SECRET an existing provider keeps its secret.
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:?set KEYCLOAK_URL, e.g. https://auth.example.com}"
REALM="${KEYCLOAK_REALM:-mavericks}"
ADMIN_USER="${KEYCLOAK_ADMIN:-admin}"
ADMIN_PASS="${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD}"
export GOOGLE_CLIENT_ID="${GOOGLE_CLIENT_ID:?set GOOGLE_CLIENT_ID (the OAuth client of Google Cloud Console)}"
export GOOGLE_CLIENT_SECRET="${GOOGLE_CLIENT_SECRET:-}"
export GOOGLE_ENABLED="${GOOGLE_ENABLED:-true}"
# The alias the gateway and the console look for (google_signin.go).
export ALIAS="google"
export FLOW="mavericks google first login"
export SUBFLOW="mavericks google create or link"

log() { echo "  $*" >&2; }
# CURL_OPTS: as in the other keycloak-*-setup.sh scripts, for a deployment
# whose DNS or certificate does not exist yet.
curl() { command curl ${CURL_OPTS:-} "$@"; }
urlenc() { python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }

T="$(curl -sf --max-time 30 -X POST \
  "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -d client_id=admin-cli -d grant_type=password \
  --data-urlencode "username=${ADMIN_USER}" \
  --data-urlencode "password=${ADMIN_PASS}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')"
API="${KEYCLOAK_URL}/admin/realms/${REALM}"
auth=(-H "Authorization: Bearer $T")
json=(-H "Content-Type: application/json")

# ── The first-login flow ─────────────────────────────────────────────────────
# Review profile (only when Google left out a name the realm requires), then
# one of: create the account when no account has the address, or set the
# existing one — Keycloak's "automatically link" recipe.
if ! curl -sf "${auth[@]}" "${API}/authentication/flows" \
    | python3 -c 'import sys,json,os; sys.exit(0 if any(f["alias"]==os.environ["FLOW"] for f in json.load(sys.stdin)) else 1)'; then
  log "creating the first-login flow \"${FLOW}\""
  curl -sf -o /dev/null -X POST "${API}/authentication/flows" "${auth[@]}" "${json[@]}" \
    -d "{\"alias\":\"${FLOW}\",\"description\":\"Google: link the account with the same verified address, or create one\",\"providerId\":\"basic-flow\",\"topLevel\":true,\"builtIn\":false}"
  F="$(urlenc "$FLOW")"
  curl -sf -o /dev/null -X POST "${API}/authentication/flows/${F}/executions/execution" "${auth[@]}" "${json[@]}" \
    -d '{"provider":"idp-review-profile"}'
  curl -sf -o /dev/null -X POST "${API}/authentication/flows/${F}/executions/flow" "${auth[@]}" "${json[@]}" \
    -d "{\"alias\":\"${SUBFLOW}\",\"type\":\"basic-flow\",\"description\":\"Create the account, or set the existing one\",\"provider\":\"registration-page-form\"}"
  S="$(urlenc "$SUBFLOW")"
  for provider in idp-create-user-if-unique idp-auto-link; do
    curl -sf -o /dev/null -X POST "${API}/authentication/flows/${S}/executions/execution" "${auth[@]}" "${json[@]}" \
      -d "{\"provider\":\"${provider}\"}"
  done
fi

# Requirements and the review-profile setting, every run: a flow edited by
# hand in the admin console comes back to what the gateway expects.
F="$(urlenc "$FLOW")"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
curl -sf "${auth[@]}" "${API}/authentication/flows/${F}/executions" > "$TMP"
python3 - "$TMP" <<'PY' | while IFS=$'\t' read -r kind id body; do
import json, os, sys
want = {"idp-review-profile": "REQUIRED", "idp-create-user-if-unique": "ALTERNATIVE", "idp-auto-link": "ALTERNATIVE"}
for e in json.load(open(sys.argv[1])):
    req = want.get(e.get("providerId")) or ("REQUIRED" if e.get("displayName") == os.environ["SUBFLOW"] else None)
    if req and e.get("requirement") != req:
        e["requirement"] = req
        print("requirement", e["id"], json.dumps(e), sep="\t")
    if e.get("providerId") == "idp-review-profile" and not e.get("authenticationConfig"):
        print("config", e["id"], json.dumps({"alias": "mavericks-google-review-profile",
              "config": {"update.profile.on.first.login": "missing"}}), sep="\t")
PY
  if [ "$kind" = requirement ]; then
    curl -sf -o /dev/null -X PUT "${API}/authentication/flows/${F}/executions" "${auth[@]}" "${json[@]}" -d "$body"
  else
    curl -sf -o /dev/null -X POST "${API}/authentication/executions/${id}/config" "${auth[@]}" "${json[@]}" -d "$body"
  fi
done

# ── The identity provider ────────────────────────────────────────────────────
code="$(curl -s -o "$TMP" -w '%{http_code}' "${auth[@]}" "${API}/identity-provider/instances/${ALIAS}")"
if [ "$code" = 404 ] && [ -z "$GOOGLE_CLIENT_SECRET" ]; then
  echo "set GOOGLE_CLIENT_SECRET: there is no Google provider yet to keep a secret from" >&2
  exit 1
fi
[ "$code" = 404 ] && echo '{}' > "$TMP"
python3 - "$TMP" <<'PY'
import json, os, sys
path = sys.argv[1]
idp = json.load(open(path))
cfg = idp.get("config") or {}
cfg.update({
    "clientId": os.environ["GOOGLE_CLIENT_ID"],
    # Google's address, name and verified-address claims.
    "defaultScope": "openid email profile",
    # IMPORT: names come from Google on the first login only, so one changed
    # in the console stays.
    "syncMode": "IMPORT",
    "hideOnLoginPage": "false",
    "guiOrder": "1",
})
if os.environ["GOOGLE_CLIENT_SECRET"]:
    cfg["clientSecret"] = os.environ["GOOGLE_CLIENT_SECRET"]
# Keycloak masks the stored secret on read; sent back masked, it keeps it.
idp.update({
    "alias": os.environ["ALIAS"], "providerId": "google", "displayName": "Google",
    "enabled": os.environ["GOOGLE_ENABLED"] != "false",
    # Google verifies the addresses it returns: the account is created with
    # its address verified, and linked to the realm account that has it.
    "trustEmail": True, "storeToken": False,
    "firstBrokerLoginFlowAlias": os.environ["FLOW"],
    "config": cfg,
})
json.dump(idp, open(path, "w"))
PY
if [ "$code" = 404 ]; then
  curl -sf -o /dev/null -X POST "${API}/identity-provider/instances" "${auth[@]}" "${json[@]}" --data @"$TMP"
else
  curl -sf -o /dev/null -X PUT "${API}/identity-provider/instances/${ALIAS}" "${auth[@]}" "${json[@]}" --data @"$TMP"
fi

log "Google sign-in $([ "$GOOGLE_ENABLED" = false ] && echo "configured, switched off" || echo on) (client ${GOOGLE_CLIENT_ID})."
log "Google's authorised redirect URI: ${KEYCLOAK_URL}/realms/${REALM}/broker/${ALIAS}/endpoint"
log "The console sees the change within a minute (the gateway asks Keycloak, and remembers the answer that long)."
