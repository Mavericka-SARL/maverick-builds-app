/**
 * A developer stops a running trigger run from the Triggers tab's execution
 * log (owner request, 2026-10-10). Mocked API — what cancelling does to the
 * instance, its steps, its execution and who is notified is covered by
 * TestCancelRunningWorkflowInstance in internal/gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("a developer cancels a running run with a reason; ended runs offer no Cancel", async ({ page }) => {
  let cancelled = false;
  let posted: { url: string; body: unknown } | null = null;
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const run = (id: string, status: string, instance: string) => ({
    id, rule_id: "rule-1", application_id: "app-1", status, trigger_payload: {}, instance_id: instance,
    started_at: "2026-10-10T16:26:13Z",
  });
  await page.route("**/api/automation/executions", (route) => route.fulfill(json([
    run("cbb903fb-0000-4000-8000-000000000001", cancelled ? "cancelled" : "running", "839f6845-0000-4000-8000-000000000001"),
    run("aaaa0000-0000-4000-8000-000000000002", "completed", "bbbb0000-0000-4000-8000-000000000002"),
  ])));
  await page.route("**/api/workflow/instances/*/cancel", (route) => {
    posted = { url: route.request().url(), body: route.request().postDataJSON() };
    cancelled = true;
    return route.fulfill(json({ status: "cancelled", notified: 2 }));
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Triggers", exact: true }).click();

  await expect(page.getByRole("button", { name: "Cancel run cbb903fb" })).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Cancel run aaaa0000" })).toHaveCount(0);

  await page.getByRole("button", { name: "Cancel run cbb903fb" }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toContainText("Cancel this run?");
  await dialog.getByLabel("Reason").fill("started by mistake");
  await dialog.getByRole("button", { name: "Cancel run" }).click();

  await expect.poll(() => posted).not.toBeNull();
  expect(posted!.url).toContain("/api/workflow/instances/839f6845-0000-4000-8000-000000000001/cancel");
  expect(posted!.body).toEqual({ reason: "started by mistake" });
  // The list is read again and the run reads cancelled, with no Cancel left.
  await expect(page.getByRole("button", { name: "Cancel run cbb903fb" })).toHaveCount(0);
  await expect(page.locator("td", { hasText: "cancelled" })).toBeVisible();
});
