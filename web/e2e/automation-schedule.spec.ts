/**
 * Scheduled automation is reachable from the Triggers tab (docs audit,
 * 2026-09-25): the engine ran cron rules since migration 059, but the form
 * offered no "schedule" trigger, so a developer could not create one. Mocked
 * API — the scheduler itself is covered by Go tests in internal/workflow.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const manualWf = {
  id: "wf-manual", application_id: "app-1", name: "Month-end close", description: "", trigger_event: "manual",
  status: "published", step_count: 2, created_at: "2026-09-01T00:00:00Z", updated_at: "2026-09-01T00:00:00Z",
};
const formWf = { ...manualWf, id: "wf-form", name: "Expense approval", trigger_event: "form.submit" };

test("a developer creates a schedule rule for a manual workflow", async ({ page }) => {
  let posted: Record<string, unknown> | null = null;
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/developer/workflows?*", (route) => route.fulfill(json([manualWf, formWf])));
  await page.route("**/api/automation/rules", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    posted = route.request().postDataJSON();
    return route.fulfill(json({ id: "rule-new", ...posted, enabled: true, created_at: "2026-09-25T00:00:00Z" }));
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Triggers", exact: true }).click();
  await page.getByRole("button", { name: "New rule" }).click();
  await page.getByPlaceholder("rule_name").fill("close_monthly");

  const selects = page.locator(".mvx-panel select");
  const workflow = selects.nth(0);
  const triggerType = selects.nth(1);

  // An event workflow fixes the trigger type.
  await workflow.selectOption("wf-form");
  await expect(triggerType).toBeDisabled();

  // A manual workflow may run by hand or on a schedule — nothing else.
  await workflow.selectOption("wf-manual");
  await expect(triggerType).toBeEnabled();
  await expect(triggerType.locator("option")).toHaveText([/^manual/, /^schedule/]);
  await triggerType.selectOption("schedule");

  const create = page.getByRole("button", { name: "Create rule" });
  await expect(create).toBeDisabled(); // no cron expression yet
  await page.getByPlaceholder("0 6 * * 1").fill("0 6 1 * *");
  await page.getByPlaceholder("UTC").fill("Europe/Paris");
  await selects.nth(2).selectOption("fire_now");
  await create.click();

  await expect.poll(() => posted).not.toBeNull();
  expect(posted).toMatchObject({
    name: "close_monthly", trigger_type: "schedule", workflow_def_id: "wf-manual",
    cron_expr: "0 6 1 * *", timezone: "Europe/Paris", misfire_policy: "fire_now",
  });
});
