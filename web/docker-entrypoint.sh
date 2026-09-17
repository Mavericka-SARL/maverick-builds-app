#!/bin/sh
# Writes the runtime configuration the SPA reads before its bundle loads.
#
# Dropped into /docker-entrypoint.d/, which the nginx image runs before
# starting nginx. Vite inlines import.meta.env at build time, so without this
# every environment would need its own image and the artifact under test would
# never be the artifact deployed. See web/src/config.ts.
#
# Written to /tmp/runtime rather than the web root because the container runs
# with a read-only root filesystem; nginx serves this path via an alias.
set -eu

mkdir -p /tmp/runtime

# Values default to empty; src/config.ts falls back to its own defaults for
# anything blank, and treats DEV_MODE as false unless explicitly "true".
cat > /tmp/runtime/config.js <<EOF
window.__MAVERICKS_CONFIG__ = {
  DEV_MODE: "${DEV_MODE:-false}",
  KEYCLOAK_URL: "${KEYCLOAK_URL:-}",
  KEYCLOAK_REALM: "${KEYCLOAK_REALM:-}",
  KEYCLOAK_CLIENT_ID: "${KEYCLOAK_CLIENT_ID:-}"
};
EOF

echo "runtime config written (DEV_MODE=${DEV_MODE:-false}, realm=${KEYCLOAK_REALM:-<unset>})"
