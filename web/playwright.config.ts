import { defineConfig, devices } from "@playwright/test";

// The console's port. CI runs the suite in shards side by side on one
// self-hosted host, each on a port of its own (E2E_PORT); 5173 otherwise.
const port = Number(process.env.E2E_PORT || 5173);

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: [["html", { open: "never" }], ["list"]],

  use: {
    baseURL: `http://localhost:${port}`,
    trace: "on-first-retry",
    // Dev mode: persona is controlled via localStorage
    storageState: undefined,
  },

  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],

  webServer: {
    command: `VITE_DEV_MODE=true npm run dev -- --port ${port} --strictPort`,
    url: `http://localhost:${port}`,
    reuseExistingServer: !process.env.CI,
    timeout: 30_000,
  },
});
