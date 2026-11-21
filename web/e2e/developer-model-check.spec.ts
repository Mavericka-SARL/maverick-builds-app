/**
 * Developer Models › Model check lists what in the revision is allowed but costs
 * time later, most serious first; a finding the developer keeps is ignored
 * with a reason and moves under "Show ignored"; a metric save answers with
 * the findings about the metric, shown above the Metrics list
 * (2026-11-21, from an inventory model built by hand).
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
const WORKING = { id: "rev-w", name: "Working", created_at: "2026-09-01T00:00:00Z", is_active: false };

const pinned = {
  rule: "pinned_members", severity: "warning", title: "Fixed member list on a business-maintained dimension",
  object: { kind: "widget", id: "w1", name: "Plan by store", dashboard: "Cockpit" },
  message: "Plan by store on dashboard Cockpit shows a fixed list of 2 location members, and business users maintain location: members they add later will not appear there.",
  fix: "Clear the list to show every member, and let the dashboard's selector bar narrow it.",
  key: "pinned_members|cockpit|plan by store|location", ignored: false,
};
const schedule = {
  rule: "schedule_without_values", severity: "error", title: "Schedule cannot start its workflow",
  object: { kind: "automation_rule", id: "r1", name: "Monday cycle" },
  message: "Trigger Monday cycle starts workflow Weekly cycle on a schedule, and the workflow needs week when it starts: a schedule brings no values, so every run fails.",
  fix: "Give week a default in the workflow's Context (a formula such as a date setting or TODAY()), or make it optional.",
  key: "schedule_without_values|monday cycle", ignored: false,
};

test("model check: findings, ignore with a reason, and the warnings a save answers with", async ({ page }) => {
  let ignored: { key: string; reason: string } | undefined;
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([WORKING])));
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json([{
    id: "t-1", name: "Acme", plan: "test", created_at: "2026-01-01T00:00:00Z",
    applications: [{ id: "app-1", name: "Planning", mode: "planning", status: "active",
      models: [{ id: "model-1", name: "Finance Model", storage_type: "oltp", active_revision: "Live", is_default: true, revisions: [WORKING] }] }],
  }])));
  await page.route("**/api/developer/model-check?*", (route) => route.fulfill(json({
    revision_id: "rev-w",
    findings: ignored ? [schedule, { ...pinned, ignored: true, ignore_reason: ignored.reason, ignored_by: "Lena" }] : [schedule, pinned],
    counts: { error: 1, warning: ignored ? 0 : 1, info: 0, ignored: ignored ? 1 : 0 },
  })));
  await page.route("**/api/developer/model-check/ignores", async (route) => {
    ignored = route.request().postDataJSON() as { key: string; reason: string };
    await route.fulfill(json({ ignored: true }));
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Developer Models", exact: true }).click();
  const section = page.getByRole("region", { name: "Model check" });
  await expect(section.getByText("1 errors")).toBeVisible({ timeout: 15_000 }); // the working revision's counts at a glance
  await section.getByRole("button", { name: "Show the findings" }).click();
  const cards = page.getByRole("article");
  await expect(cards).toHaveCount(2);
  await expect(cards.first()).toContainText("Schedule cannot start its workflow"); // the error first
  await expect(cards.nth(1)).toContainText("Widget · Plan by store (dashboard Cockpit)");

  const card = cards.nth(1);
  await card.getByRole("button", { name: "Ignore…" }).click();
  await expect(card.getByRole("button", { name: "Ignore", exact: true })).toBeDisabled();
  await card.getByLabel("Why it is kept").fill("the cockpit shows the two flagship stores on purpose");
  await card.getByRole("button", { name: "Ignore", exact: true }).click();
  await expect.poll(() => ignored?.key).toBe(pinned.key);
  await expect(cards).toHaveCount(1);
  await page.getByRole("button", { name: "Show ignored (1)" }).click();
  await expect(page.getByText("Ignored by Lena: the cockpit shows the two flagship stores on purpose")).toBeVisible();
});

test("a metric save shows the findings about the metric above the list", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([WORKING])));
  await page.route("**/api/developer/metrics/m1*", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    return route.fulfill(json({ status: "ok", recalc: [], warnings: [{
      rule: "total_from_blanks", severity: "warning", title: "A total worked out from blanks",
      object: { kind: "metric", id: "m1", name: "revenue" },
      message: "revenue is a calculated pick-list whose total rows are worked out with its formula.",
      fix: "Set its Total to none (Developer › Metrics).", key: "total_from_blanks|revenue", ignored: false,
    }] }));
  });
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Metrics", exact: true }).click();
  await page.getByRole("button", { name: "Edit metric revenue" }).click();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("revenue is a calculated pick-list whose total rows are worked out with its formula.")).toBeVisible({ timeout: 15_000 });
  await page.getByRole("button", { name: "Dismiss" }).click();
  await expect(page.getByText("A total worked out from blanks")).toHaveCount(0);
});
