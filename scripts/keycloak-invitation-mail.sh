#!/usr/bin/env bash
# Give an existing realm the product's invitation mail: the "maverickbuilds"
# email theme, and the three account attributes the mail reads, declared in
# the realm's user profile for administrators only.
#
# A new install gets both from its realm file. Keycloak reads that file only
# on first start, so a realm made before release 2026.10.26.1 needs this once;
# running it again changes nothing. Until then invitations still go out, in
# Keycloak's own words ("Your administrator has just requested...").
#
# The gateway marks each account before its invitation (pkg/keycloak
# MarkInvitation): mvx_invitation (signup or invite), mvx_invited_by and
# mvx_workspace. Keycloak 24 drops an attribute its user profile does not
# declare, and the mail then reads neutrally.
#
#   KEYCLOAK_URL=https://auth.example.com \
#   KEYCLOAK_ADMIN_PASSWORD=... \
#   bash scripts/keycloak-invitation-mail.sh
#
# The theme's files must be on Keycloak's disk first: Compose mounts them;
# on Kubernetes they come with the keycloak-theme ConfigMap, read when the
# pod starts.
set -euo pipefail

KEYCLOAK_URL="${KEYCLOAK_URL:?set KEYCLOAK_URL, e.g. https://auth.example.com}"
REALM="${KEYCLOAK_REALM:-mavericks}"
ADMIN_USER="${KEYCLOAK_ADMIN:-admin}"
ADMIN_PASS="${KEYCLOAK_ADMIN_PASSWORD:?set KEYCLOAK_ADMIN_PASSWORD}"

log() { echo "  $*" >&2; }

# CURL_OPTS: as the other keycloak-*.sh scripts, to reach a deployment whose
# DNS or certificate does not exist yet.
curl() { command curl ${CURL_OPTS:-} "$@"; }

T="$(curl -sf --max-time 30 -X POST \
  "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
  -d client_id=admin-cli -d grant_type=password \
  --data-urlencode "username=${ADMIN_USER}" \
  --data-urlencode "password=${ADMIN_PASS}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')"
API="${KEYCLOAK_URL}/admin/realms/${REALM}"

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

# The user profile: add what is missing, keep everything else as it is.
curl -sf -H "Authorization: Bearer $T" "$API/users/profile" > "$TMP"
if python3 - "$TMP" <<'PY'
import json, sys
path = sys.argv[1]
profile = json.load(open(path))
have = {a["name"] for a in profile["attributes"]}
added = False
for name, label in [("mvx_invitation", "Invitation (signup or invite)"),
                    ("mvx_invited_by", "Invited by"),
                    ("mvx_workspace", "Invited to workspace")]:
    if name not in have:
        profile["attributes"].append({"name": name, "displayName": label,
            "permissions": {"view": ["admin"], "edit": ["admin"]}, "multivalued": False})
        added = True
json.dump(profile, open(path, "w"))
sys.exit(0 if added else 1)
PY
then
  curl -sf -X PUT -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
    --data-binary "@$TMP" "$API/users/profile" > /dev/null
  log "declared the invitation attributes in the user profile"
else
  log "the user profile already declares the invitation attributes"
fi

# The email theme. Read-modify-write: a realm PUT takes a whole
# representation, and a partial one would blank every other setting.
curl -sf -H "Authorization: Bearer $T" "$API" > "$TMP"
if python3 - "$TMP" <<'PY'
import json, sys
path = sys.argv[1]
realm = json.load(open(path))
if realm.get("emailTheme") == "maverickbuilds":
    sys.exit(1)
realm["emailTheme"] = "maverickbuilds"
json.dump(realm, open(path, "w"))
PY
then
  curl -sf -X PUT -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
    --data-binary "@$TMP" "$API" > /dev/null
  log "the realm's email theme is now maverickbuilds"
else
  log "the realm already uses the maverickbuilds email theme"
fi
