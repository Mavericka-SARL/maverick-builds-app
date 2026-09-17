import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

// Unsaved edits had two silent escape routes: leaving the page (nothing
// registered a beforeunload handler anywhere in the app) and switching
// context inside it (the access-rules panel reset its draft on user change).
// Role→dashboard assignment had no draft at all — each checkbox PUT the whole
// list immediately, so a mis-click was live for every member of the role.
test.describe("unsaved changes are guarded", () => {
  test("role dashboard assignment is a draft applied by one Save", async ({ page }) => {
    await mockApi(page);
    // Registered AFTER mockApi: Playwright matches the most recently added
    // handler first, so a route added before the catch-all never fires — and
    // the "nothing was written yet" assertion below would pass vacuously.
    const puts: string[] = [];
    await page.route("**/api/business-admin/roles/*/dashboards", async (route) => {
      if (route.request().method() === "PUT") puts.push(route.request().url());
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok" }) });
    });
    await loadAs(page, "finance");

    await page.getByRole("button", { name: /^roles$/i }).first().click();
    await page.waitForTimeout(400);
    await page.getByRole("button", { name: /configure/i }).first().click();
    await page.waitForTimeout(400);

    const checkbox = page.getByRole("checkbox").first();
    await expect(checkbox).toBeVisible();
    await checkbox.click();

    // Nothing is written yet — the change is a draft.
    expect(puts, "toggling a dashboard must not write immediately").toHaveLength(0);

    const bar = page.getByText(/unsaved changes/i).first();
    await expect(bar).toBeVisible();

    await page.getByRole("button", { name: /save dashboard access/i }).click();
    await page.waitForTimeout(500);
    expect(puts.length, "Save applies the pending assignment").toBeGreaterThan(0);
  });

  test("discarding is confirmed rather than silent", async ({ page }) => {
    await mockApi(page);
    await loadAs(page, "finance");

    await page.getByRole("button", { name: /^roles$/i }).first().click();
    await page.waitForTimeout(400);
    await page.getByRole("button", { name: /configure/i }).first().click();
    await page.waitForTimeout(400);

    await page.getByRole("checkbox").first().click();
    await expect(page.getByText(/unsaved changes/i).first()).toBeVisible();

    // Collapsing the role would drop the draft — it must ask first, through
    // the shared alertdialog (native confirm() is banned in this codebase).
    // Scoped to the main area: the sidebar now has its own "Collapse sidebar"
    // toggle, which /collapse/i would match first.
    await page.locator(".mvx-app-shell__main").getByRole("button", { name: /collapse/i }).first().click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText(/discard/i);
  });
});
