#!/usr/bin/env bash
# Set up and check the Compose stack. docs/SELF_HOSTING.md walks through it.
#
#   ./setup.sh env                          create .env; generate every secret
#   ./setup.sh keycloak                     after `docker compose up -d`: the
#                                           provisioning account, sign-in
#                                           hardening and Keycloak's e-mail
#   ./setup.sh admin EMAIL "First Last"     the first platform administrator
#                                           (asks for a password, or reads
#                                           ADMIN_PASSWORD)
#   ./setup.sh check                        is everything answering?
#   ./setup.sh restore [RUN|latest]         list the backup runs, or put one
#                                           back: every database it holds
#
# Every step but restore is safe to repeat. Run it on the server itself: it
# reaches the stack through the proxy on 127.0.0.1, with the real host names,
# so it works before DNS does and still exercises TLS and routing.
#
# Needs bash, curl, openssl and python3 (the Keycloak scripts use python3 to
# read JSON), and the docker compose plugin.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
ENV_FILE="$HERE/.env"
cd "$HERE"

log() { echo "==> $*" >&2; }
die() { echo "error: $*" >&2; exit 1; }

# The value of KEY in .env, as Compose reads it: everything after the first
# "=", with one pair of surrounding quotes removed.
env_get() {
  [ -f "$ENV_FILE" ] || return 0
  local v
  v="$(sed -n "s/^$1=//p" "$ENV_FILE" | tail -1)"
  v="${v%\"}"; v="${v#\"}"; v="${v%\'}"; v="${v#\'}"
  printf '%s' "$v"
}

# Replace KEY's line in .env, or append one. awk rather than sed -i, which
# differs between GNU and BSD.
env_set() {
  local tmp
  tmp="$(mktemp "$ENV_FILE.XXXXXX")"
  awk -v k="$1" -v v="$2" '
    BEGIN { done = 0 }
    index($0, k "=") == 1 { print k "=" v; done = 1; next }
    { print }
    END { if (!done) print k "=" v }' "$ENV_FILE" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$ENV_FILE"
}

need_env() {
  [ -f "$ENV_FILE" ] || die ".env not found — run ./setup.sh env first"
}

# curl through the local proxy, with the real host name in the URL, the SNI
# and the Host header. A certificate from Caddy's local CA is not trusted
# here, so it is not verified in that mode.
proxy_curl_opts() {
  local opts=""
  for host in "$(env_get CONSOLE_HOST)" "$(env_get AUTH_HOST)"; do
    opts="$opts --resolve ${host}:443:127.0.0.1"
  done
  [ "$(env_get CADDY_CERTS)" = "local_certs" ] && opts="$opts -k"
  printf '%s' "${SETUP_CURL_OPTS:-$opts}"
}

cmd_env() {
  if [ -f "$ENV_FILE" ]; then
    log ".env exists; filling in only the secrets that are still empty"
  else
    cp "$HERE/.env.example" "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    log "created .env from .env.example"
  fi
  # Hex: safe inside a connection URL and in every config syntax involved.
  local key
  for key in POSTGRES_PASSWORD KEYCLOAK_ADMIN_PASSWORD MINIO_ROOT_PASSWORD \
             INTEGRATION_CRED_KEY SECRETS_ENCRYPTION_KEY; do
    if [ -z "$(env_get "$key")" ]; then
      env_set "$key" "$(openssl rand -hex 32)"
      log "generated $key"
    fi
  done
  if [ -z "$(env_get BACKUP_S3_SECRET_KEY)" ] \
     && [ "$(env_get BACKUP_S3_URL)" = "http://minio:9000" ]; then
    env_set BACKUP_S3_SECRET_KEY "$(env_get MINIO_ROOT_PASSWORD)"
    log "backups go to this stack's own object storage"
  fi
  cat >&2 <<EOF

Next:
  1. Edit $ENV_FILE: CONSOLE_HOST, AUTH_HOST and the SMTP_* lines at least.
  2. Save a copy of it in a password manager — backups are useless without it.
  3. docker compose up -d --build
  4. ./setup.sh keycloak
EOF
}

cmd_keycloak() {
  need_env
  local auth_host console_host curl_opts url secret
  auth_host="$(env_get AUTH_HOST)"
  console_host="$(env_get CONSOLE_HOST)"
  curl_opts="$(proxy_curl_opts)"
  url="https://${auth_host}"

  log "waiting for Keycloak at ${url}"
  local i
  for i in $(seq 1 60); do
    # shellcheck disable=SC2086
    if curl -sf $curl_opts -o /dev/null "${url}/realms/mavericks/.well-known/openid-configuration"; then
      break
    fi
    [ "$i" = 60 ] && die "Keycloak did not answer at ${url} within 5 minutes — see 'docker compose logs keycloak proxy'"
    sleep 5
  done

  log "provisioning service account"
  secret="$(KEYCLOAK_URL="$url" KEYCLOAK_ADMIN_PASSWORD="$(env_get KEYCLOAK_ADMIN_PASSWORD)" \
    CURL_OPTS="$curl_opts" bash "$ROOT/scripts/keycloak-provisioning-setup.sh")"
  [ -n "$secret" ] || die "the provisioning script returned no client secret"
  env_set KEYCLOAK_ADMIN_CLIENT_SECRET "$secret"
  log "stored KEYCLOAK_ADMIN_CLIENT_SECRET in .env"

  log "enabling brute-force protection on the master realm"
  KEYCLOAK_URL="$url" KEYCLOAK_ADMIN_PASSWORD="$(env_get KEYCLOAK_ADMIN_PASSWORD)" \
    CURL_OPTS="$curl_opts" bash "$ROOT/scripts/keycloak-master-hardening.sh"

  if [ -n "$(env_get SMTP_HOST)" ]; then
    log "configuring Keycloak's e-mail (invitations, password resets)"
    KEYCLOAK_URL="$url" KEYCLOAK_ADMIN_PASSWORD="$(env_get KEYCLOAK_ADMIN_PASSWORD)" \
      CURL_OPTS="$curl_opts" \
      SMTP_HOST="$(env_get SMTP_HOST)" SMTP_PORT="$(env_get SMTP_PORT)" \
      SMTP_USER="$(env_get SMTP_USERNAME)" SMTP_PASSWORD="$(env_get SMTP_PASSWORD)" \
      SMTP_FROM="$(env_get SMTP_FROM)" SMTP_FROM_NAME="${SMTP_FROM_NAME:-maverickbuilds.app}" \
      SMTP_TEST_TO="${SMTP_TEST_TO:-}" \
      bash "$ROOT/scripts/keycloak-smtp-setup.sh"
  else
    log "SMTP_HOST is empty: Keycloak cannot send invitations, so the console cannot add users yet"
  fi

  log "restarting the gateway with the new secret"
  docker compose up -d gateway
  log "done — next: ./setup.sh admin you@example.com \"Your Name\" (sign in at https://${console_host})"
}

cmd_admin() {
  need_env
  local email="${1:-}" name="${2:-Platform Administrator}" password="${ADMIN_PASSWORD:-}"
  [ -n "$email" ] || die "usage: ./setup.sh admin EMAIL \"First Last\""
  if [ -z "$password" ]; then
    local again
    read -r -s -p "Password for ${email}: " password; echo >&2
    read -r -s -p "Again: " again; echo >&2
    [ "$password" = "$again" ] || die "the passwords differ"
  fi
  [ "${#password}" -ge 12 ] || die "use at least 12 characters"

  KEYCLOAK_URL="https://$(env_get AUTH_HOST)" \
  KEYCLOAK_ADMIN_PASSWORD="$(env_get KEYCLOAK_ADMIN_PASSWORD)" \
  CURL_OPTS="$(proxy_curl_opts)" \
  ADMIN_EMAIL="$email" ADMIN_PASSWORD="$password" ADMIN_NAME="$name" \
  PSQL="docker compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U mavericks -d mavericks" \
    bash "$ROOT/scripts/bootstrap-platform-admin.sh"
  log "sign in at https://$(env_get CONSOLE_HOST) as ${email}"
}

cmd_check() {
  need_env
  local curl_opts console auth failed=0
  curl_opts="$(proxy_curl_opts)"
  console="https://$(env_get CONSOLE_HOST)"
  auth="https://$(env_get AUTH_HOST)"

  check() { # description, expected HTTP status, URL
    local code
    # shellcheck disable=SC2086
    code="$(curl -s $curl_opts -o /dev/null -w '%{http_code}' --max-time 15 "$3" || true)"
    if [ "$code" = "$2" ]; then
      printf '  ok    %s\n' "$1"
    else
      printf '  FAIL  %s (%s answered %s, expected %s)\n' "$1" "$3" "${code:-nothing}" "$2"
      failed=1
    fi
  }
  echo "Through the proxy:"
  check "console"                             200 "$console/"
  check "gateway (refuses an anonymous call)" 401 "$console/api/me"
  check "sign-in service"                     200 "$auth/realms/mavericks/.well-known/openid-configuration"

  echo "Configuration:"
  if [ -n "$(env_get KEYCLOAK_ADMIN_CLIENT_SECRET)" ]; then
    echo "  ok    user provisioning configured"
  else
    echo "  FAIL  KEYCLOAK_ADMIN_CLIENT_SECRET is empty — run ./setup.sh keycloak"; failed=1
  fi
  if [ -n "$(env_get SMTP_HOST)" ]; then
    echo "  ok    e-mail relay set ($(env_get SMTP_HOST))"
  else
    echo "  FAIL  SMTP_HOST is empty — the console cannot invite users"; failed=1
  fi

  echo "Backups:"
  local listing
  listing="$(docker compose exec -T backup sh -c \
    'mc alias set b "$MINIO_URL" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null && mc ls "b/$BACKUP_BUCKET/pg/"' 2>/dev/null || true)"
  if [ -n "$listing" ]; then
    echo "$listing" | tail -3 | sed 's/^/  ok    /'
  else
    echo "  none yet — the first runs at $(env_get BACKUP_HOUR):00 UTC; take one now with:"
    echo "        docker compose exec backup backup-schedule.sh now"
  fi

  [ "$failed" = 0 ] || exit 1
}

# Object names under pg/ in the backup bucket, one per line.
backup_objects() {
  docker compose exec -T backup sh -c \
    'mc alias set b "$MINIO_URL" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null && mc ls --json "b/$BACKUP_BUCKET/pg/"' \
    | python3 -c 'import sys, json; [print(json.loads(l)["key"]) for l in sys.stdin if l.strip()]'
}

# A run is one pass of pg-backup.sh: every database it dumped carries the same
# timestamp, so restoring a run restores the platform, its tenants and the
# sign-in accounts as they were at the same moment.
cmd_restore() {
  need_env
  local run="${1:-}" objects runs files f db answer
  objects="$(backup_objects)"
  runs="$(echo "$objects" | sed -nE 's/^backup-.+-([0-9]{8}T[0-9]{6}Z)\.dump$/\1/p' | sort -u)"
  [ -n "$runs" ] || die "no backups found — see 'docker compose logs backup'"
  if [ -z "$run" ]; then
    echo "Backup runs (UTC), oldest first:"
    echo "$runs" | sed 's/^/  /'
    echo "Restore one with: ./setup.sh restore <run>   (or: ./setup.sh restore latest)"
    return
  fi
  [ "$run" = "latest" ] && run="$(echo "$runs" | tail -1)"
  files="$(echo "$objects" | grep -E -- "-${run}\.dump$" || true)"
  [ -n "$files" ] || die "no backup run ${run}"

  echo "This replaces the current data with the backup run ${run}:" >&2
  echo "$files" | sed 's/^/  /' >&2
  echo "Everything written since then is lost. The console is offline meanwhile." >&2
  if [ "${CONFIRM_RESTORE:-}" != "yes" ]; then
    read -r -p "Type RESTORE to continue: " answer
    [ "$answer" = "RESTORE" ] || die "nothing was changed"
  fi

  log "stopping everything that writes to the databases"
  docker compose stop proxy gateway integration keycloak
  for f in $files; do
    db="${f#backup-}"; db="${db%-${run}.dump}"
    log "restoring ${db}"
    docker compose exec -T -e CONFIRM_RESTORE=yes -e OBJECT="$f" -e DB="$db" backup sh -c \
      'RESTORE_DATABASE_URL="$(echo "$DATABASE_URL" | sed -E "s#(://[^/]+)/[^?]*#\1/$DB#")" pg-restore.sh "$OBJECT"'
  done
  log "starting the stack again"
  docker compose up -d
  log "restored ${run}; run ./setup.sh check"
}

case "${1:-}" in
  env)      cmd_env ;;
  keycloak) cmd_keycloak ;;
  admin)    shift; cmd_admin "$@" ;;
  check)    cmd_check ;;
  restore)  shift; cmd_restore "$@" ;;
  *)        sed -n '2,20p' "$HERE/setup.sh" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
