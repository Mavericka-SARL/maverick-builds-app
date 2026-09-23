/**
 * White-labelling: a configured brand changes the tab title, the sidebar
 * name and logo, and the colour tokens for everyone in the tenant; the
 * Branding tab (commercial and enterprise) edits it and previews the
 * derived colours; the community edition sees the gate and the default look.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, chooseTenant, commercialLicense, acmeBrand } from "./mocks";

test("community: default look, and the Branding tab shows the gate", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await expect(page).toHaveTitle("maverickbuilds.app");
  await expect(page.getByTestId("brand-logo")).toHaveCount(0);
  // The default head is the product's own mark, not a tenant's logo.
  await expect(page.locator(".mvx-app-shell__mark")).toBeVisible();
  // …and the tab wears the product's own icon, both links of it.
  const icons = await page.locator("link[rel~='icon']").evaluateAll((ls) => ls.map((l) => (l as HTMLLinkElement).getAttribute("href")));
  expect(icons).toEqual(["/favicon.svg", "/favicon.ico"]);
  await page.getByRole("button", { name: "Branding", exact: true }).click();
  await chooseTenant(page);
  await expect(page.locator(".mvx-feature-gate")).toContainText("White-labelling");
  await expect(page.locator(".mvx-feature-gate")).toContainText("requires the Commercial edition");
});

test("a configured brand is applied everywhere the product shows its name", async ({ page }) => {
  await mockApi(page, { license: commercialLicense, brand: acmeBrand });
  await loadAs(page, "developer");
  await expect(page).toHaveTitle("Acme Planning");
  await expect(page.getByRole("heading", { name: "Acme Planning" })).toBeVisible();
  await expect(page.getByTestId("brand-logo")).toBeVisible();
  // …and it REPLACES the product mark, the one thing allowed to.
  await expect(page.locator(".mvx-app-shell__mark")).toHaveCount(0);
  // The tab icon goes with it — every link, or the product's own mark would
  // still be what a tenant's browser tab shows.
  const icons = await page.locator("link[rel~='icon']").evaluateAll((ls) => ls.map((l) => (l as HTMLLinkElement).getAttribute("href")));
  expect(icons.every((h) => h?.startsWith("data:image/svg+xml"))).toBe(true);
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
  await chooseTenant(page);
  const tab = page.getByTestId("branding");
  await expect(tab.getByLabel("Product name")).toHaveValue("Acme Planning");
  await expect(tab.getByLabel("Custom domain")).toHaveValue("planning.acme.test");
  await expect(tab.getByTestId("logo-preview")).toBeVisible();
  await expect(tab.getByTestId("brand-swatches").locator("span")).toHaveCount(6);
  await tab.getByLabel("Brand colour", { exact: true }).fill("#b91c1c");
  await expect(tab.getByTestId("brand-swatches").locator("span").nth(3)).toHaveAttribute("title", "#b91c1c");
  await expect(tab.getByRole("button", { name: "Remove branding" })).toBeVisible();
});
