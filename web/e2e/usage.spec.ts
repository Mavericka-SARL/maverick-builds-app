/**
 * Admin › Usage: a platform admin sees a row per tenant, with the period
 * selectable; other editions see the gate. Numbers come straight from the
 * API — the tab formats, it never computes.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, enterpriseLicense } from "./mocks";

test("community: the Usage tab shows the gate", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Usage", exact: true }).click();
  await expect(page.locator(".mvx-feature-gate")).toContainText("Usage analytics");
  await expect(page.getByTestId("usage")).toHaveCount(0);
});

test("enterprise: one row per tenant with the period selector", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  let lastPeriod = "";
  await page.route("**/api/admin/usage*", async (route) => {
    lastPeriod = new URL(route.request().url()).searchParams.get("period") ?? "";
    await route.fallback();
  });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Usage", exact: true }).click();
  const view = page.getByTestId("usage");
  await expect(view).toBeVisible();
  const acme = view.getByRole("row", { name: /Acme Corp/ });
  await expect(acme).toContainText("7 / 12");
  await expect(acme).toContainText("2 / 3 / 9");
  await expect(acme).toContainText("10,119 / 31,168");
  await expect(acme).toContainText("94 / 4 / 0");
  await expect(acme).toContainText("40.0 MB");
  await expect(view.getByRole("row", { name: /Meridian/ })).toContainText("never");
  await expect.poll(() => lastPeriod).toBe("30d");
  await view.getByRole("button", { name: "90 days" }).click();
  await expect.poll(() => lastPeriod).toBe("90d");
});
