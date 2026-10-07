import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

// User › Models with a model other than the default opened: the banner, the
// "Working here" badge and the Active Revision panel all name that model and
// its live revision — not the application's default (reported 2026-10-07:
// working in "Business admin guide", the panel showed "Learn the platform").
test("Models names the opened model and its revision, not the application's default", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/apps", route => route.fulfill({
    json: [{
      id: "app-1", name: "Planning", mode: "planning", workspace_name: "Main",
      model_name: "Finance Model", active_revision: "FY2026 Budget",
      models: [
        { id: "model-1", name: "Finance Model", is_default: true, active_revision: "FY2026 Budget" },
        { id: "model-2", name: "Sales Model", is_default: false, active_revision: "Sales FY2026" },
      ],
    }],
  }));
  await page.addInitScript(() => {
    localStorage.setItem("selected_app_id", "app-1");
    localStorage.setItem("selected_model_id", "model-2");
  });
  await loadAs(page, "dept_head");
  await page.getByRole("button", { name: "Models", exact: true }).click();

  const banner = page.locator(".mvx-context-banner").filter({ hasText: "Working in:" });
  await expect(banner).toContainText("Planning · Sales Model", { timeout: 15_000 });
  await expect(banner).toContainText("revision Sales FY2026");
  const row = (name: string) => page.locator(".mvx-panel").filter({ hasText: name });
  await expect(row("Sales Model")).toContainText("Working here");
  await expect(row("Finance Model").getByRole("button", { name: "Open" })).toBeVisible();
  const panel = page.locator(".mvx-admin-model");
  await expect(panel.locator(".mvx-admin-model__name")).toHaveText("Sales Model");
  await expect(panel.locator(".mvx-admin-revision__name")).toHaveText("Sales FY2026");
});
