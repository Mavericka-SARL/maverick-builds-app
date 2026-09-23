import { defineConfig, devices } from "@playwright/test";

/**
 * The real-login suite (e2e-keycloak/): a genuine Keycloak PKCE sign-in
 * against a gateway running with DEV_MODE=false, then an authenticated API
 * call — the one path the mocked smoke suite cannot cover. It needs a
 * Postgres and a Keycloak with the dev realm; CI brings them up from
 * deploy/docker/docker-compose.ci-keycloak.yml, a developer usually has the
 * dev stack (`make dev-up`) already.
 *
 * Ports are overridable so the suite can run beside a developer's own
 * gateway (8080) and console (5173): E2E_GATEWAY_PORT / E2E_WEB_PORT. The
 * realm's mavericks-web client allows redirects to localhost:5173 and
 * localhost:3000 only, so E2E_WEB_PORT is one of those two. DATABASE_URL,
 * KEYCLOAK_URL and VITE_KEYCLOAK_URL pass through to the servers as set.
 */
const gatewayPort = process.env.E2E_GATEWAY_PORT ?? "8080";
const webPort = process.env.E2E_WEB_PORT ?? "5173";

export default defineConfig({
  testDir: "./e2e-keycloak",
  globalSetup: "./e2e-keycloak/global-setup.ts",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: [["list"]],
  timeout: 60_000,
  use: {
    baseURL: `http://localhost:${webPort}`,
    trace: "on-first-retry",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
  webServer: [
    {
      command: "go run ./cmd/gateway",
      cwd: "../",
      url: `http://localhost:${gatewayPort}/healthz`,
      reuseExistingServer: !process.env.CI,
      // `go run` compiles first: a cold module cache on CI takes a while.
      timeout: 180_000,
      env: { DEV_MODE: "false", HTTP_PORT: gatewayPort },
    },
    {
      command: `VITE_DEV_MODE=false npm run dev -- --port ${webPort} --strictPort`,
      url: `http://localhost:${webPort}`,
      reuseExistingServer: !process.env.CI,
      timeout: 60_000,
      env: { API_PROXY: `http://localhost:${gatewayPort}` },
    },
  ],
});
