// Runtime configuration.
//
// Vite inlines `import.meta.env` at BUILD time, so a bundle built with
// VITE_KEYCLOAK_URL=https://auth.example.com is pinned to that value forever.
// Taken literally that means one image per environment, and a promotion from
// staging to production becomes a rebuild rather than a redeploy — which also
// means the artifact you tested is not the artifact you ship.
//
// Instead the container writes /config.js at startup from its environment and
// index.html loads it before the bundle, so a single image is promoted
// unchanged. See web/docker-entrypoint.sh.
//
// The import.meta.env fallback keeps `npm run dev` behaving exactly as before:
// vite serves .env.development, and public/config.js declares an empty object
// so the <script> tag resolves in dev too.

type ConfigKey = "DEV_MODE" | "KEYCLOAK_URL" | "KEYCLOAK_REALM" | "KEYCLOAK_CLIENT_ID";

declare global {
  interface Window {
    __MAVERICKS_CONFIG__?: Partial<Record<ConfigKey, string>>;
  }
}

function resolve(key: ConfigKey, buildTime: string | undefined, fallback: string): string {
  const runtime = window.__MAVERICKS_CONFIG__?.[key];
  // envsubst leaves "${KEYCLOAK_URL}" in place when the variable is unset in
  // the container. Treat that as absent rather than handing a literal
  // placeholder to Keycloak, which would fail with a confusing URL error.
  if (runtime && runtime !== "" && !runtime.startsWith("${")) return runtime;
  return buildTime ?? fallback;
}

export const config = {
  // Defaults to FALSE. Dev mode disables JWKS validation and enables persona
  // switching, so the safe default for anything that forgot to set it is off.
  devMode: resolve("DEV_MODE", import.meta.env.VITE_DEV_MODE, "false") === "true",
  keycloakUrl: resolve("KEYCLOAK_URL", import.meta.env.VITE_KEYCLOAK_URL, "http://localhost:8180"),
  keycloakRealm: resolve("KEYCLOAK_REALM", import.meta.env.VITE_KEYCLOAK_REALM, "mavericks"),
  keycloakClientId: resolve("KEYCLOAK_CLIENT_ID", import.meta.env.VITE_KEYCLOAK_CLIENT_ID, "mavericks-web"),
};
