import { defineConfig, devices } from "@playwright/test";

// A separate config from playwright.config.ts on purpose: the default config
// always starts the dev server with VITE_DEV_MODE=true (persona-based auth),
// which is exactly the path this suite needs to NOT be on — it drives a real
// Keycloak PKCE login (P0-1) against a gateway with JWKS validation wired
// up. Preconditions this config does not start for you: the docker-compose
// infra (Postgres + Keycloak), e.g.
//   docker compose -f ../deploy/docker/docker-compose.dev.yml up -d postgres keycloak
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
    baseURL: "http://localhost:5173",
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
      url: "http://localhost:8080/healthz",
      reuseExistingServer: !process.env.CI,
      timeout: 30_000,
      env: { DEV_MODE: "false" },
    },
    {
      command: "VITE_DEV_MODE=false npm run dev",
      url: "http://localhost:5173",
      reuseExistingServer: !process.env.CI,
      timeout: 30_000,
    },
  ],
});
