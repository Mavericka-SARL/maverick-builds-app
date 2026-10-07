/**
 * The developer header must say WHICH application and model the selected
 * revision belongs to. Many models have a revision called "Working", and with
 * nothing selected the console falls back to a default revision that may be
 * another application's; a badge that only read "Working" once let objects be
 * created in the wrong model without any visible sign.
 */
import { test, expect } from "@playwright/test";
import { loadAs, mockApi } from "./mocks";

test("developer context bar names the application and model beside the revision badge", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  const bar = page.locator(".mvx-context-bar");
  await expect(bar).toContainText("Model", { timeout: 15_000 });
  // From the mocked GET /api/developer/model: app_name · model_name.
  await expect(bar).toContainText("Planning · Finance Model");
  await expect(bar).toContainText("Revision");
});

// The User and Business Admin screens name it too: an application can hold
// several models (sign-up's holds the tour and three guides), and the
// revision alone did not say which one a person was reading.
for (const persona of ["dept_head", "finance"]) {
  test(`${persona}: the context bar names the application and model the dashboards come from`, async ({ page }) => {
    await mockApi(page);
    await page.route("**/api/apps", route => route.fulfill({
      json: [{
        id: "app-1", name: "Planning", workspace_name: "Main", model_name: "Finance Model",
        models: [
          { id: "model-1", name: "Finance Model", is_default: true, active_revision: "FY2026 Budget" },
          { id: "model-2", name: "Sales Model", is_default: false, active_revision: "FY2026" },
        ],
      }],
    }));
    await loadAs(page, persona);
    const bar = page.locator(".mvx-context-bar");
    await expect(bar).toContainText("Planning · Finance Model", { timeout: 15_000 });
    await expect(bar).toContainText("FY2026 Budget");
  });
}
