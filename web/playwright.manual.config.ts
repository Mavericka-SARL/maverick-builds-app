import { defineConfig, devices } from "@playwright/test";

// Recaptures the developer manual's screenshots (docs/developer-manual/img)
// from a live console: a gateway in DEV_MODE with the reference demo seeded
// (make dev-admin, make demo) and the web app on :5173. Not part of `npm run
// e2e` — it writes files into the docs, it does not test anything.
//
//   npm run manual:screenshots
//   (cd ../docs/developer-manual && sh build.sh)
export default defineConfig({
  testDir: "./e2e-manual",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  timeout: 45_000,

  use: {
    ...devices["Desktop Chrome"],
    baseURL: "http://localhost:5173",
    viewport: { width: 1400, height: 900 },
    deviceScaleFactor: 1,
    colorScheme: "light",
  },

  webServer: {
    command: "VITE_DEV_MODE=true npm run dev",
    url: "http://localhost:5173",
    reuseExistingServer: true,
    timeout: 30_000,
  },
});
