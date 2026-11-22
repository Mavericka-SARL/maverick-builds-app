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
  expect(icons).toEqual(["/favicon.svg?v=3", "/favicon.ico?v=3"]);
  await page.getByRole("button", { name: "Branding", exact: true }).click();
  await chooseTenant(page);
  await expect(page.locator(".mvx-feature-gate")).toContainText("White-labelling");
  await expect(page.locator(".mvx-feature-gate")).toContainText("requires the Commercial Edition");
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

// Saving puts the brand in force at once for the tenant's own people — the
// sidebar's logo and name too, not only the colours (found 2026-11-21 in the
// Commercial key run: the logo waited for a reload). A platform
// administrator editing another tenant's brand keeps their own look.
const LOGO_SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="120" height="32"><rect width="120" height="32" fill="#0f766e"/></svg>';
async function echoSavedBrand(page: import("@playwright/test").Page) {
  await page.route((url) => url.pathname === "/api/admin/branding", async (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    const body = route.request().postDataJSON();
    return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ...body, configured: true }) });
  });
}

test("commercial: a tenant admin's saved logo and name replace the product mark at once", async ({ page }) => {
  await mockApi(page, { license: commercialLicense });
  await echoSavedBrand(page);
  await loadAs(page, "tenant_admin");
  await expect(page.locator(".mvx-app-shell__mark")).toBeVisible();
  await page.getByRole("button", { name: "Branding", exact: true }).click();
  const tab = page.getByTestId("branding");
  await tab.getByLabel("Product name").fill("Acme Planning");
  await tab.getByLabel("Logo file").setInputFiles({ name: "logo.svg", mimeType: "image/svg+xml", buffer: Buffer.from(LOGO_SVG) });
  await tab.getByRole("button", { name: "Save branding" }).click();
  await expect(page.getByTestId("brand-logo")).toBeVisible();
  await expect(page.locator(".mvx-app-shell__mark")).toHaveCount(0);
  await expect(page).toHaveTitle("Acme Planning");
});

test("commercial: a platform admin saving another tenant's brand keeps the product mark", async ({ page }) => {
  await mockApi(page, { license: commercialLicense });
  await echoSavedBrand(page);
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Branding", exact: true }).click();
  await chooseTenant(page);
  const tab = page.getByTestId("branding");
  await tab.getByLabel("Product name").fill("Acme Planning");
  await tab.getByLabel("Logo file").setInputFiles({ name: "logo.svg", mimeType: "image/svg+xml", buffer: Buffer.from(LOGO_SVG) });
  await tab.getByRole("button", { name: "Save branding" }).click();
  await expect(tab.getByRole("button", { name: "Saved" })).toBeVisible();
  await expect(page.getByTestId("brand-logo")).toHaveCount(0);
  await expect(page.locator(".mvx-app-shell__mark")).toBeVisible();
  await expect(page).toHaveTitle("maverickbuilds.app");
});
