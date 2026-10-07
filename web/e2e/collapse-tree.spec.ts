import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

// Applications › an application's models and a model's revisions collapse,
// everything starts expanded, and what a person collapsed stays so after a
// reload (remembered in this browser).
test("applications and models collapse, and stay collapsed after a reload", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "tenant_admin");
  await expect(page.getByRole("heading", { name: "Applications" })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("FY2026 Forecast")).toBeVisible();

  await page.getByRole("button", { name: "Collapse model Finance Model" }).click();
  await expect(page.getByText("FY2026 Forecast")).toHaveCount(0);
  await expect(page.getByText("2 revisions · active: FY2026 Budget")).toBeVisible();

  await page.getByRole("button", { name: "Collapse application Planning" }).click();
  await expect(page.getByText("Finance Model")).toHaveCount(0);

  await page.reload();
  await expect(page.getByRole("heading", { name: "Applications" })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("button", { name: "Expand application Planning" })).toHaveAttribute("aria-expanded", "false");
  await expect(page.getByText("Finance Model")).toHaveCount(0);

  await page.getByRole("button", { name: "Expand application Planning" }).click();
  await expect(page.getByText("Finance Model")).toBeVisible();
  await expect(page.getByText("FY2026 Forecast")).toHaveCount(0);
  await page.getByRole("button", { name: "Expand model Finance Model" }).click();
  await expect(page.getByText("FY2026 Forecast")).toBeVisible();
});
