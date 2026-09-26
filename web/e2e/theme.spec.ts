/**
 * Light / dark theme: a per-person choice in the account menu, kept in this
 * browser. Light until someone opts in; "System" follows the OS live; the
 * choice is applied by index.html before the bundle runs, so a dark page
 * never paints white first.
 */
import { test, expect, type Page } from "@playwright/test";

async function open(page: Page) {
  await page.addInitScript(() => localStorage.setItem("dev_persona", "developer"));
  const actor = { user_id: "00000000-0000-0000-0000-000000000009", email: "signed.in@example.com", display_name: "Signed In Person", roles: ["developer"] };
  for (const path of ["**/api/me", "**/api/admin/me"]) {
    await page.route(path, (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(actor) }));
  }
  await page.goto("/");
  await expect(page.locator(".mvx-app-shell__brand")).toBeVisible({ timeout: 15_000 });
}

const html = (page: Page) => page.locator("html");
const bodyBg = (page: Page) => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
const choose = async (page: Page, name: "Light" | "Dark" | "System") => {
  if (!(await page.locator(".mvx-user-menu__popover").isVisible())) await page.getByRole("button", { name: /^Account/ }).click();
  await page.getByRole("menuitemradio", { name }).click();
};

test("light by default; Dark switches the whole page and is remembered", async ({ page }) => {
  await open(page);
  await expect(html(page)).toHaveAttribute("data-theme", "light");
  expect(await bodyBg(page)).toBe("rgb(247, 248, 251)");

  await choose(page, "Dark");
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await expect(page.getByRole("menuitemradio", { name: "Dark" })).toHaveAttribute("aria-checked", "true");
  expect(await bodyBg(page)).toBe("rgb(11, 17, 32)");

  // Applied before first paint: with the app bundle blocked, only index.html's
  // inline script can have set it.
  await page.route("**/src/main.tsx", (route) => route.abort());
  await page.reload();
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
});

test("System follows the operating system, live", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  await open(page);
  // Not chosen yet: an OS in dark mode does not flip anyone's console.
  await expect(html(page)).toHaveAttribute("data-theme", "light");

  await choose(page, "System");
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await page.emulateMedia({ colorScheme: "light" });
  await expect(html(page)).toHaveAttribute("data-theme", "light");

  await choose(page, "Light");
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(html(page)).toHaveAttribute("data-theme", "light");
});

test("a tenant brand colour gets its own dark-theme shades", async ({ page }) => {
  await page.route("**/api/branding", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ product_name: "Acme", tagline: "", logo_data_url: "", favicon_data_url: "", brand_color: "#0f766e", configured: true, source: "tenant" }) }),
  );
  await open(page);
  const token = (name: string) => page.evaluate((n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim(), name);
  await expect.poll(() => token("--color-brand-500")).toBe("#0f766e");
  const lightTint = await token("--color-brand-50");

  await choose(page, "Dark");
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  // Still the tenant's colour, but tints are dark and text shades lighter.
  expect(await token("--color-brand-500")).toBe("#0f766e");
  expect(await token("--color-brand-50")).not.toBe(lightTint);
  expect(await token("--color-brand-50")).toBe("#112531");
  expect(await token("--color-brand-600")).toBe("#4b9892");
});
