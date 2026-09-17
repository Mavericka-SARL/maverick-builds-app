/**
 * Admin › Audit Log gains an enterprise panel: export the caller's scope as
 * CSV or JSON Lines (unbound by the 200-row table) and set retention. Other
 * editions see the gate above the table they already had.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, enterpriseLicense } from "./mocks";

test("community: the gate sits above the audit table, which still renders", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Audit Log", exact: true }).click();
  await expect(page.locator(".mvx-feature-gate")).toContainText("Audit export");
  await expect(page.getByTestId("audit-export")).toHaveCount(0);
  await expect(page.getByPlaceholder("Filter by event, actor, or resource…")).toBeVisible();
});

test("enterprise: export requests the chosen format and range; retention has a floor", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  let exportURL = "";
  await page.route("**/api/admin/audit/export*", (route) => {
    exportURL = route.request().url();
    return route.fulfill({ status: 200, contentType: "text/csv", headers: { "Content-Disposition": 'attachment; filename="audit-test.csv"' }, body: "occurred_at,category\n" });
  });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Audit Log", exact: true }).click();
  const panel = page.getByTestId("audit-export");
  await expect(panel).toBeVisible();
  await expect(panel).toContainText("at least 30 days");

  await panel.getByLabel("Export format").selectOption("jsonl");
  await panel.getByLabel("Export from").fill("2026-09-01");
  await panel.getByLabel("Export to").fill("2026-09-16");
  await panel.getByRole("button", { name: "Export" }).click();
  await expect.poll(() => exportURL).toContain("format=jsonl");
  expect(exportURL).toContain("since=2026-09-01");
  expect(exportURL).toContain("until=2026-09-16");

  await expect(panel.getByLabel("Retention days")).toHaveValue("0");
});
