/**
 * White-labelling: a configured brand changes the tab title, the sidebar
 * name and logo, and the colour tokens for everyone in the tenant; the
 * Branding tab (commercial and enterprise) edits it and previews the
 * derived colours; the community edition sees the gate and the default look.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, commercialLicense, acmeBrand } from "./mocks";

test("community: default look, and the Branding tab shows the gate", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await expect(page).toHaveTitle("Mavericks");
  await expect(page.getByTestId("brand-logo")).toHaveCount(0);
  await page.getByRole("button", { name: "Branding", exact: true }).click();
  await expect(page.locator(".mvx-feature-gate")).toContainText("White-labelling");
  await expect(page.locator(".mvx-feature-gate")).toContainText("requires the Commercial edition");
});

test("a configured brand is applied everywhere the product shows its name", async ({ page }) => {
  await mockApi(page, { license: commercialLicense, brand: acmeBrand });
  await loadAs(page, "developer");
  await expect(page).toHaveTitle("Acme Planning");
  await expect(page.getByRole("heading", { name: "Acme Planning" })).toBeVisible();
  await expect(page.getByTestId("brand-logo")).toBeVisible();
  const brand500 = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-brand-500").trim());
  expect(brand500).toBe("#0f766e");
  const brand700 = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-brand-700").trim());
  expect(brand700).not.toBe("#0f766e");
  expect(brand700).toMatch(/^#[0-9a-f]{6}$/);
});

test("commercial: the Branding tab edits the brand and previews derived colours", async ({ page }) => {
  await mockApi(page, { license: commercialLicense, brand: acmeBrand });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Branding", exact: true }).click();
  const tab = page.getByTestId("branding");
  await expect(tab.getByLabel("Product name")).toHaveValue("Acme Planning");
  await expect(tab.getByLabel("Custom domain")).toHaveValue("planning.acme.test");
  await expect(tab.getByTestId("logo-preview")).toBeVisible();
  await expect(tab.getByTestId("brand-swatches").locator("span")).toHaveCount(6);
  await tab.getByLabel("Brand colour", { exact: true }).fill("#b91c1c");
  await expect(tab.getByTestId("brand-swatches").locator("span").nth(3)).toHaveAttribute("title", "#b91c1c");
  await expect(tab.getByRole("button", { name: "Remove branding" })).toBeVisible();
});
