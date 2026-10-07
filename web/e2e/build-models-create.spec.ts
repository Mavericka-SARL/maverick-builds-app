/**
 * The developer's sidebar and Developer › Models: Developer's groups fold (and stay
 * folded), AI Developer leads Build and Models ends it, and creating an
 * application or a model sits at the TOP of both screens that offer it —
 * Developer › Models for someone who is also tenant admin, and Tenant admin ›
 * Applications. The API is mocked (e2e/mocks.ts).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const sidebar = (page: Page) => page.getByRole("navigation", { name: "Primary" });
const top = async (page: Page, selector: string) =>
  page.locator(selector).first().evaluate((el) => el.getBoundingClientRect().top);

// Developer › Models lists what the developer reaches; the mocks answer it with
// nothing unless told, so it gets the tenant the admin screens see.
const tenants = [{
  id: "tenant-1", name: "Acme Corp", plan: "enterprise", created_at: "2026-01-01T00:00:00Z",
  applications: [{
    id: "app-1", name: "Planning", mode: "planning", status: "active",
    models: [{
      id: "model-1", name: "Finance Model", storage_type: "oltp", active_revision: "FY2026 Budget", is_default: true,
      revisions: [{ id: "rev-1", name: "FY2026 Budget", created_at: "2026-01-01T00:00:00Z" }],
    }],
  }],
}];

async function withApplications(page: Page) {
  await mockApi(page);
  await page.route((url) => url.pathname === "/api/developer/applications", (route) => route.fulfill({ json: tenants }));
}

test("Developer opens with AI Developer, ends with Models, and folds away until unfolded", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  const build = sidebar(page).getByRole("button", { name: "Developer", exact: true });
  await expect(build).toHaveAttribute("aria-expanded", "true");
  const items = await page.locator(".mvx-sidebar-nav__group", { has: page.getByRole("button", { name: "Developer", exact: true }) }).locator(".mvx-sidebar-nav__item .mvx-sidebar-nav__label").allTextContents();
  expect(items[0]).toBe("AI Developer");
  expect(items[items.length - 1]).toBe("Models");

  await build.click();
  await expect(build).toHaveAttribute("aria-expanded", "false");
  await expect(sidebar(page).getByRole("button", { name: "Metrics", exact: true })).toHaveCount(0);
  // Govern is a group of its own and stays open.
  await expect(sidebar(page).getByRole("button", { name: "Users", exact: true })).toBeVisible();

  await page.reload();
  await expect(sidebar(page).getByRole("button", { name: "Developer", exact: true })).toHaveAttribute("aria-expanded", "false");
  await expect(sidebar(page).getByRole("button", { name: "Metrics", exact: true })).toHaveCount(0);

  await sidebar(page).getByRole("button", { name: "Developer", exact: true }).click();
  await expect(sidebar(page).getByRole("button", { name: "Metrics", exact: true })).toBeVisible();
});

test("the icon-only rail shows every item, folded or not", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await sidebar(page).getByRole("button", { name: "Developer", exact: true }).click();
  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await expect(sidebar(page).getByRole("button", { name: "Metrics", exact: true })).toHaveCount(1);
  await page.getByRole("button", { name: "Expand sidebar" }).click();
  await expect(sidebar(page).getByRole("button", { name: "Metrics", exact: true })).toHaveCount(0);
});

test("a developer alone gets no New application or New model in Developer › Models", async ({ page }) => {
  await withApplications(page);
  await loadAs(page, "developer");
  await sidebar(page).getByRole("button", { name: "Models", exact: true }).click();
  await expect(page.locator(".mvx-admin-model__name", { hasText: "Finance Model" })).toBeVisible();
  await expect(page.getByRole("button", { name: "New application" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "New model" })).toHaveCount(0);
});

test("a developer who is also tenant admin creates a model from the top of Developer › Models", async ({ page }) => {
  await withApplications(page);
  await loadAs(page, "dev_admin");
  await sidebar(page).getByRole("button", { name: "Models", exact: true }).first().click();
  const newModel = page.getByRole("button", { name: "New model" });
  await expect(newModel).toBeVisible();
  expect(await top(page, ".mvx-toolbar")).toBeLessThan(await top(page, ".mvx-admin-object"));

  await newModel.click();
  await expect(page.getByRole("combobox").filter({ hasText: "Planning" })).toBeVisible();
  await page.getByPlaceholder("e.g. Budget 2027").fill("Budget 2027");
  const posted = page.waitForRequest((r) => r.method() === "POST" && new URL(r.url()).pathname === "/api/admin/models");
  await page.getByRole("button", { name: "Create model" }).click();
  const req = await posted;
  expect(req.postDataJSON()).toMatchObject({ application_id: "app-1", name: "Budget 2027" });
  // Addressed to the application's own tenant.
  expect(req.headers()["x-tenant-id"]).toBe("tenant-1");
});

test("Tenant admin › Applications offers New application and Add model above the lists", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "tenant_admin");
  await expect(page.getByRole("button", { name: "New application" })).toBeVisible();
  const newApp = await page.getByRole("button", { name: "New application" }).evaluate((el) => el.getBoundingClientRect().top);
  const addModel = await page.getByRole("button", { name: "Add model" }).first().evaluate((el) => el.getBoundingClientRect().top);
  expect(newApp).toBeLessThan(await top(page, ".mvx-admin-object:not(.mvx-admin-object--tenant)"));
  expect(addModel).toBeLessThan(await top(page, ".mvx-admin-model"));
});
