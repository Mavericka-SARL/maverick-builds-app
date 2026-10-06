/**
 * A selected model the gateway refuses (404 MODEL_NOT_OPEN: deleted, its
 * access revoked, another account signed in) is dropped and the request
 * retried, so the console opens the application's default model instead of
 * failing every request. The gateway used to swap in the default silently.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("a refused model selection is dropped and the console opens the default", async ({ page }) => {
  await mockApi(page);
  let refused = 0;
  // Registered after mockApi, so it is asked first.
  await page.route((url) => url.pathname.startsWith("/api/"), async (route) => {
    if (route.request().headers()["x-model-id"] === "stale-model") {
      refused++;
      return route.fulfill({ status: 404, contentType: "application/json",
        body: JSON.stringify({ error: "the selected model is not one you can open in this application", code: "MODEL_NOT_OPEN" }) });
    }
    return route.fallback();
  });
  await page.addInitScript(() => {
    if (!sessionStorage.getItem("seeded")) {
      localStorage.setItem("selected_model_id", "stale-model");
      sessionStorage.setItem("seeded", "1");
    }
  });
  await loadAs(page, "developer");
  await expect(page.getByRole("navigation", { name: "Primary" })).toBeVisible();
  expect(refused).toBeGreaterThan(0);
  expect(await page.evaluate(() => localStorage.getItem("selected_model_id"))).toBeNull();
  await expect(page.getByText("MODEL_NOT_OPEN")).toHaveCount(0);
});
