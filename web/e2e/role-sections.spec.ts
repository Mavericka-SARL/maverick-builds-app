/**
 * Roles are additive, and the rule is that a combination of roles ADDS tabs
 * to ONE console — it never offers a second console. Each role contributes
 * its sidebar group(s); a multi-role user sees the union in one sidebar.
 *
 * /api/me is mocked rather than seeding multi-role personas, because the
 * built-in dev personas all carry exactly one role and the point here is the
 * client's behaviour given a multi-role actor.
 */
import { test, expect, type Page } from "@playwright/test";

async function signInAs(page: Page, roles: string[]) {
  await page.addInitScript(() => {
    localStorage.setItem("dev_persona", "platform_admin");
  });
  await page.route("**/api/me", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        user_id: "00000000-0000-0000-0000-000000000001",
        email: "multi@example.com",
        display_name: "Multi Role",
        roles,
      }),
    });
  });
}

// The sidebar head is the product mark for everyone: it says WHOSE product
// this is, never which console is showing. What a person holds is the groups
// below it, which is what each test here checks.
const mark = (page: Page) => page.locator(".mvx-app-shell__brand-name .mvx-app-shell__mark");
const nav = (page: Page, name: string) => page.getByRole("button", { name, exact: true });

test("there is no console switcher: one console, always", async ({ page }) => {
  await signInAs(page, ["platform_admin", "developer"]);
  await page.goto("/");
  await expect(page.locator(".mvx-app-shell__brand")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByLabel("Console", { exact: true })).toHaveCount(0);
});

test("tenant_admin + business_admin: tenant-admin tabs sit in the business admin console", async ({ page }) => {
  await signInAs(page, ["business_admin", "tenant_admin"]);
  await page.goto("/");
  await expect(mark(page)).toBeVisible({ timeout: 15_000 });
  // Business-admin groups…
  await expect(nav(page, "Dashboards")).toBeVisible();
  await expect(nav(page, "Access Rules")).toBeVisible();
  // …and the tenant-admin group, in the same sidebar.
  await expect(nav(page, "Applications")).toBeVisible();
  await expect(nav(page, "Users")).toBeVisible();
  await expect(nav(page, "Audit Log")).toBeVisible();
  // Lands on the everyday screen, and Applications (model export/import) is one click away.
  await nav(page, "Applications").click();
  await expect(page.getByRole("heading", { name: "Applications" })).toBeVisible({ timeout: 15_000 });
});

test("platform_admin + developer: Build and Platform groups in one sidebar, Users only once", async ({ page }) => {
  await signInAs(page, ["platform_admin", "developer"]);
  await page.goto("/");
  await expect(mark(page)).toBeVisible({ timeout: 15_000 });
  await expect(nav(page, "Metrics")).toBeVisible();
  await expect(nav(page, "Infrastructure")).toBeVisible();
  await expect(nav(page, "Users")).toHaveCount(1);
});

test("business_user + business_admin: the admin Plan group supersedes the user one", async ({ page }) => {
  await signInAs(page, ["business_user", "business_admin"]);
  await page.goto("/");
  await expect(mark(page)).toBeVisible({ timeout: 15_000 });
  await expect(nav(page, "Dashboards")).toHaveCount(1);
  await expect(nav(page, "My History")).toHaveCount(0);
  await expect(nav(page, "History")).toBeVisible();
});

test("a single-role user still opens exactly where they always did", async ({ page }) => {
  await signInAs(page, ["developer"]);
  await page.goto("/");
  await expect(mark(page)).toBeVisible({ timeout: 15_000 });
  await expect(nav(page, "Metrics")).toBeVisible();
  await expect(nav(page, "Users")).toBeVisible();
});

// The head is the one part of the sidebar that does NOT change with roles —
// a rule that only white-labelling is allowed to break (see white-label.spec).
for (const roles of [["business_user"], ["developer"], ["tenant_admin"], ["platform_admin", "developer", "business_admin"]]) {
  test(`the sidebar head is the product mark for ${roles.join(" + ")}`, async ({ page }) => {
    await signInAs(page, roles);
    await page.goto("/");
    await expect(mark(page)).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole("heading", { name: "maverickbuilds.app" })).toBeVisible();
    await expect(page.locator(".mvx-app-shell__brand")).not.toContainText("console");
    await expect(page.locator(".mvx-app-shell__brand")).not.toContainText("administration");
  });
}
