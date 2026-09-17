/**
 * Plans and trials in the console: a tenant member sees their trial (and a
 * read-only workspace says why); the platform admin sees each tenant's plan
 * on its card, changes it there, and edits the catalog under Platform ›
 * Plans. The wording is the server's throughout.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, trialPlanState } from "./mocks";

test("a trial tenant sees the days left and where to change the plan", async ({ page }) => {
  await mockApi(page, { plan: trialPlanState(5) });
  await loadAs(page, "developer");
  const banner = page.getByTestId("plan-banner");
  await expect(banner).toHaveAttribute("data-state", "trial");
  await expect(banner).toContainText("Trial · 5 days left");
  await expect(banner.getByRole("link", { name: "Change plan" })).toHaveAttribute("href", "https://example.test/pricing");
});

test("a read-only tenant is told why, in the server's words", async ({ page }) => {
  await mockApi(page, { plan: trialPlanState(0, { read_only: true, code: "trial_expired", reason: "The trial ended on 1 September 2026. The workspace is read-only until the plan is changed." }) });
  await loadAs(page, "developer");
  const banner = page.getByTestId("plan-banner");
  await expect(banner).toHaveAttribute("data-state", "trial_expired");
  await expect(banner).toContainText("The trial ended on 1 September 2026");
});

test("no banner without a plan worth mentioning", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await expect(page.getByTestId("plan-banner")).toHaveCount(0);
});

test("platform admin: the tenant card shows the plan and changes it", async ({ page }) => {
  await mockApi(page, { plan: trialPlanState(5) });
  await loadAs(page, "platform_admin");
  await expect(page.getByTestId("tenant-meta-tenant-1")).toContainText("Trial · trial, 5 days left");
  await page.getByRole("button", { name: "Change plan of Acme Corp" }).click();
  const controls = page.getByTestId("tenant-plan-tenant-1");
  await controls.getByLabel("Plan").selectOption("enterprise");
  const patched = page.waitForRequest((r) => r.method() === "PATCH" && r.url().endsWith("/api/admin/tenants/tenant-1"));
  await controls.getByRole("button", { name: "Save" }).click();
  expect((await patched).postDataJSON()).toEqual({ plan: "enterprise" });
});

test("platform admin: Plans lists the catalog and saves a changed limit", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Plans", exact: true }).click();
  const tab = page.getByTestId("plans-tab");
  await expect(tab.getByTestId("plan-trial")).toContainText("Trial");
  await expect(tab.getByTestId("plan-enterprise")).toContainText("Enterprise");
  const models = tab.getByLabel("Models limit of trial");
  await expect(models).toHaveValue("3");
  await models.fill("5");
  const put = page.waitForRequest((r) => r.method() === "PUT" && r.url().endsWith("/api/admin/plans/trial"));
  await tab.getByTestId("plan-trial").getByRole("button", { name: "Save plan" }).click();
  const body = (await put).postDataJSON() as { limits: { max_models: number }; trial_days: number; self_service: boolean };
  expect(body.limits.max_models).toBe(5);
  expect(body.trial_days).toBe(14);
  expect(body.self_service).toBe(true);
});

test("a developer has no Plans tab", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await expect(page.getByRole("button", { name: "Plans", exact: true })).toHaveCount(0);
});
