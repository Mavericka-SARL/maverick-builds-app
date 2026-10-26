/**
 * The designer names a widget's type as its palette does: a records list is
 * "Records", not the internal "form" (2026-10-26), on the widget, in its
 * properties and to a screen reader.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("a records widget is called Records in the designer", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Dashboards", exact: true }).click();
  const row = page.getByRole("button", { name: "OPEX form 2", exact: true }).locator("xpath=ancestor::div[contains(@class,'mvx-admin-object')][1]");
  await row.getByRole("button", { name: "Design", exact: true }).click();

  const widget = page.locator(".mvx-canvas-widget").first();
  await expect(widget).toContainText("RECORDS", { timeout: 15_000 });
  await expect(widget).not.toContainText("FORM");
  await expect(widget).toHaveAttribute("aria-label", /^Records widget: /);
  await widget.click();
  await expect(page.locator(".mvx-badge, [class*=badge]").filter({ hasText: /^Records$/ }).first()).toBeVisible();
});
