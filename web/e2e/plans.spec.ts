/**
 * Plans in the console: a read-only workspace says why (in the server's
 * words); the platform admin sees each tenant's plan on its card, changes it
 * there, and edits the catalog under Platform › Plans. Nothing speaks of a
 * trial: a plan bounds how much, never for how long.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, smallPlanState, overStoragePlanState } from "./mocks";

test("a basic workspace that has filled its space is read-only and told where to go, not to change plan", async ({ page }) => {
  await mockApi(page, { plan: overStoragePlanState() });
  await loadAs(page, "developer");
  const banner = page.getByTestId("plan-banner");
  await expect(banner).toHaveAttribute("data-state", "over_limit");
  await expect(banner).toContainText("100 MB of storage; this tenant uses 104 MB");
  await expect(banner).toContainText("run the platform on your own infrastructure");
  await expect(banner).not.toContainText("Change plan");
  await expect(banner.getByRole("link", { name: "Learn more" })).toHaveAttribute("href", "https://example.test/pricing");
});

test("a read-only tenant on a plan without a note is told why and offered the plan change", async ({ page }) => {
  await mockApi(page, { plan: smallPlanState({ read_only: true, code: "over_limit", limit_state: "over", reason: "This tenant is over its plan's limits (The Small plan allows 3 models; this tenant has 4. Change the plan to add more.). The workspace is read-only, except for deleting, until it is back within them." }) });
  await loadAs(page, "developer");
  const banner = page.getByTestId("plan-banner");
  await expect(banner).toHaveAttribute("data-state", "over_limit");
  await expect(banner).toContainText("The Small plan allows 3 models");
  await expect(banner.getByRole("link", { name: "Change plan" })).toHaveAttribute("href", "https://example.test/pricing");
});

test("a tenant within its limits has no banner", async ({ page }) => {
  await mockApi(page, { plan: smallPlanState() });
  await loadAs(page, "developer");
  await expect(page.getByTestId("plan-banner")).toHaveCount(0);
});

test("no banner without a plan worth mentioning", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await expect(page.getByTestId("plan-banner")).toHaveCount(0);
});

test("platform admin: the tenant card shows the plan and changes it", async ({ page }) => {
  await mockApi(page, { plan: smallPlanState() });
  await loadAs(page, "platform_admin");
  await expect(page.getByTestId("tenant-meta-tenant-1")).toContainText("Small");
  await expect(page.getByTestId("tenant-meta-tenant-1")).not.toContainText(/trial/i);
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
  await expect(tab.getByTestId("plan-test")).toContainText("Basic workspace");
  await expect(tab.getByLabel("Storage (MB) limit of test")).toHaveValue("100");
  await expect(tab.getByLabel("Limit note of test")).toHaveValue(/own infrastructure/);
  await expect(tab.getByTestId("plan-small")).toContainText("Small");
  await expect(tab.getByTestId("plan-enterprise")).toContainText("Enterprise");
  await expect(tab).not.toContainText(/trial/i);
  const models = tab.getByLabel("Models limit of small");
  await expect(models).toHaveValue("3");
  await models.fill("5");
  const put = page.waitForRequest((r) => r.method() === "PUT" && r.url().endsWith("/api/admin/plans/small"));
  await tab.getByTestId("plan-small").getByRole("button", { name: "Save plan" }).click();
  const body = (await put).postDataJSON() as { limits: { max_models: number }; self_service: boolean } & Record<string, unknown>;
  expect(body.limits.max_models).toBe(5);
  expect(body.self_service).toBe(false);
  expect(body).not.toHaveProperty("trial_days");
});

test("a developer has no Plans tab", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await expect(page.getByRole("button", { name: "Plans", exact: true })).toHaveCount(0);
});
