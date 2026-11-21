/**
 * The business admin's tools added 2026-11-07: Reopen on an approved run
 * (with a reason), Copy values (one member's values into another), and a
 * role's access rules in the Access Rules editor. Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

function json(body: unknown) {
  return { status: 200, contentType: "application/json", body: JSON.stringify(body) };
}

// The Business Admin group's screen: "Workflow" is also the User group's
// name for the person's own inbox and runs.
async function open(page: Page, tab: string) {
  await loadAs(page, "finance");
  const group = page.getByRole("navigation", { name: "Primary" }).locator(".mvx-sidebar-nav__group", {
    has: page.getByRole("button", { name: "Business Admin", exact: true }),
  });
  await group.getByRole("button", { name: tab, exact: true }).click();
}

test("an approved run is reopened with a reason, not by a status change", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/workflow/history", (r) => r.fulfill(json([{
    id: "inst-9", workflow_name: "Lock the approved budget", status: "completed", reopenable: true, context: {},
    started_at: "2026-11-01T10:00:00Z", completed_at: "2026-11-02T10:00:00Z",
    steps: [{ id: "s1", step_def_id: "approve", status: "completed", decision: "approve" }],
  }])));
  let reason = "";
  await page.route("**/api/workflow/instances/inst-9/reopen", async (r) => {
    reason = (r.request().postDataJSON() as { reason: string }).reason;
    await r.fulfill(json({ status: "cancelled", notified: 1 }));
  });
  await open(page, "Workflow"); // finance holds the user role: the History alone
  await expect(page.getByLabel("Change instance status")).toHaveCount(0);
  await page.getByRole("button", { name: "Reopen Lock the approved budget" }).click();
  await page.getByLabel("Reason").fill("March was locked with a wrong figure");
  await page.getByRole("button", { name: "Reopen run" }).click();
  await expect.poll(() => reason).toBe("March was locked with a wrong figure");
});

test("a business admin checks, then copies one member's values into another", async ({ page }) => {
  await mockApi(page);
  const calls: { dry_run?: boolean }[] = [];
  await page.route("**/api/business-admin/copy-values", async (r) => {
    const body = r.request().postDataJSON() as { dry_run?: boolean };
    calls.push(body);
    await r.fulfill(json({ cells: 3, dry_run: !!body.dry_run }));
  });
  await open(page, "Manage values");
  await page.getByLabel("Dimension").selectOption({ index: 1 });
  await page.getByLabel("Copy from").selectOption({ index: 1 });
  await page.getByLabel("Copy to").selectOption({ index: 1 });
  await expect(page.getByRole("button", { name: "Copy", exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "Check" }).click();
  await expect(page.getByRole("status")).toContainText("3 values will be written");
  await page.getByRole("button", { name: "Copy", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("Copied 3 values");
  expect(calls.map((c) => !!c.dry_run)).toEqual([true, false]);
});

test("a copy can be narrowed to members of another dimension and to some metrics", async ({ page }) => {
  await mockApi(page);
  const calls: { only?: Record<string, string[]>; metric_ids?: string[]; dry_run?: boolean }[] = [];
  await page.route("**/api/business-admin/copy-values", async (r) => {
    const body = r.request().postDataJSON() as { dry_run?: boolean };
    calls.push(body);
    await r.fulfill(json({ cells: 2, dry_run: !!body.dry_run }));
  });
  await open(page, "Manage values");
  await page.getByLabel("Dimension").selectOption({ index: 1 });
  await page.getByLabel("Copy from").selectOption({ index: 1 });
  await page.getByLabel("Copy to").selectOption({ index: 1 });
  const onlyDim = page.getByLabel("Only where dimension");
  await onlyDim.selectOption({ index: 1 });
  const members = page.getByRole("group", { name: /^Only these members of / });
  await members.getByRole("button").first().click();
  await expect(members.getByRole("button").first()).toHaveAttribute("aria-pressed", "true");
  const metrics = page.getByRole("group", { name: "Only these metrics" });
  if (await metrics.count()) await metrics.getByRole("button").first().click();
  await page.getByRole("button", { name: "Check" }).click();
  await expect(page.getByRole("status")).toContainText("2 values will be written");
  await page.getByRole("button", { name: "Copy", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("Copied 2 values");
  expect(calls).toHaveLength(2);
  for (const c of calls) {
    const dims = Object.keys(c.only ?? {});
    expect(dims).toHaveLength(1);
    expect(c.only![dims[0]]).toHaveLength(1);
  }
  expect(calls[0].metric_ids).toEqual(calls[1].metric_ids);
});

test("a role's access rules are edited like a person's", async ({ page }) => {
  await mockApi(page);
  let saved = "";
  await page.route("**/api/business-admin/roles/role-1/access-rules", async (r) => {
    if (r.request().method() === "PUT") saved = r.request().url();
    await r.fulfill(json([]));
  });
  await open(page, "Access Rules");
  await page.getByLabel("Select user").selectOption("role:role-1");
  await page.getByRole("button", { name: "Expand all", exact: true }).click();
  await page.getByLabel("Access level").first().selectOption("hidden");
  await page.getByRole("button", { name: "Save rules" }).click();
  await expect.poll(() => saved).toContain("/api/business-admin/roles/role-1/access-rules");
});
