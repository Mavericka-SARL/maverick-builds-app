/**
 * Smoke tests for all four consoles in dev mode (VITE_DEV_MODE=true).
 * Each test sets a persona via localStorage, navigates to the app,
 * and asserts that the correct console shell renders without errors.
 */
import { test, expect, type Page } from "@playwright/test";

async function setPersona(page: Page, persona: string) {
  // Seed only when unset so in-app persona switches (which reload the page)
  // are not overridden on the next navigation.
  await page.addInitScript((p) => {
    if (!localStorage.getItem("dev_persona")) {
      localStorage.setItem("dev_persona", p);
    }
  }, persona);
}

// ── Business User console (dept_head) ──────────────────────────────────────────
test("Business User console renders for dept_head persona", async ({ page }) => {
  await setPersona(page, "dept_head");
  await page.goto("/");
  // Business user sees the dashboard and inbox tab buttons in the sidebar
  await expect(page.getByRole("button", { name: "Dashboards" })).toBeVisible({ timeout: 10_000 });
});

// ── Business Admin console (finance) ──────────────────────────────────────────
test("Business Admin console renders for finance persona", async ({ page }) => {
  await setPersona(page, "finance");
  await page.goto("/");
  await expect(page.locator("body")).not.toHaveText(/error/i);
  // Business admin sees the approval inbox tab button
  await expect(page.getByRole("button", { name: /approve|inbox|workflow/i }).first()).toBeVisible({ timeout: 10_000 });
});

// ── Developer console (developer) ─────────────────────────────────────────────
test("Developer console renders for developer persona", async ({ page }) => {
  await setPersona(page, "developer");
  await page.goto("/");
  await expect(page.locator("body")).not.toHaveText(/error/i);
  // Developer sees the Metrics and Dimensions tab buttons
  await expect(page.getByRole("button", { name: "Metrics" })).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Dimensions" })).toBeVisible({ timeout: 10_000 });
});

// ── Platform Admin console (platform_admin) ───────────────────────────────────
test("Platform Admin console renders for platform_admin persona", async ({ page }) => {
  await setPersona(page, "platform_admin");
  await page.goto("/");
  await expect(page.locator("body")).not.toHaveText(/error/i);
  // Platform admin sees the three console tab buttons
  await expect(page.getByRole("button", { name: "Applications" })).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Users" })).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Audit Log" })).toBeVisible({ timeout: 10_000 });
});

// ── Persona switcher works across all consoles ────────────────────────────────
test("Persona switcher changes console view", async ({ page }) => {
  await setPersona(page, "dept_head");
  await page.goto("/");

  // Should show business user view initially
  await expect(page.getByRole("button", { name: "Dashboards" })).toBeVisible({ timeout: 10_000 });

  // Switch to developer persona via the switcher
  const switcher = page.getByRole("combobox", { name: "Switch persona" });
  await switcher.selectOption("developer");
  await expect(page.getByRole("button", { name: "Metrics" })).toBeVisible({ timeout: 10_000 });
});
