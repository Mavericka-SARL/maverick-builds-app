/**
 * After Developer › Models picks a revision of another application, the
 * developer tabs work in that revision's application and model at once.
 * The demo context was one cached ["demo"] entry for 60 s: Workflows then
 * asked for the previous application's workflows in the new revision and
 * listed nothing, and a dashboard Preview's writes were refused (found
 * recording how-to videos, 2026-10-21 and 2026-10-25).
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const TOUR_REV = { id: "rev-tour", name: "Tour live", created_at: "2026-09-01T00:00:00Z" };
const BUDGET_REV = { id: "rev-budget", name: "Budget 2027 draft", created_at: "2026-10-01T00:00:00Z" };

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

async function mockTwoApplications(page: Page) {
  await mockApi(page);
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json([{
    id: "t-1", name: "Acme", plan: "test", created_at: "2026-01-01T00:00:00Z",
    applications: [
      { id: "app-1", name: "Getting started", mode: "planning", status: "active",
        models: [{ id: "model-tour", name: "Learn the platform", storage_type: "oltp", active_revision: TOUR_REV.name, is_default: true, revisions: [TOUR_REV] }] },
      { id: "app-2", name: "Budgeting", mode: "planning", status: "active",
        models: [{ id: "model-budget", name: "Budget 2027", storage_type: "oltp", active_revision: BUDGET_REV.name, is_default: true, revisions: [BUDGET_REV] }] },
    ],
  }])));
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json(
    new URL(route.request().url()).searchParams.get("model_id") === "model-budget"
      ? [{ ...BUDGET_REV, is_active: true }]
      : [{ ...TOUR_REV, is_active: true }],
  )));
  await page.route("**/api/developer/model*", (route) => {
    const budget = new URL(route.request().url()).searchParams.get("revision_id") === BUDGET_REV.id;
    return route.fulfill(json({ app_name: budget ? "Budgeting" : "Getting started", model_name: budget ? "Budget 2027" : "Learn the platform",
      model_id: budget ? "model-budget" : "model-tour", metrics: [] }));
  });
  // The server answers for the request's application, and for the revision
  // it names (pinModelForRevision).
  await page.route("**/api/demo*", (route) => {
    const budget = new URL(route.request().url()).searchParams.get("revision_id") === BUDGET_REV.id
      || route.request().headers()["x-app-id"] === "app-2";
    return route.fulfill(json({
      app_id: budget ? "app-2" : "app-1", model_id: budget ? "model-budget" : "model-tour",
      revision_id: budget ? BUDGET_REV.id : TOUR_REV.id, revision: budget ? BUDGET_REV.name : TOUR_REV.name,
      scenario: budget ? BUDGET_REV.name : TOUR_REV.name, version: "draft",
      actor: { user_id: "u-1", email: "alex@example.com", display_name: "Alex", roles: ["developer"] },
    }));
  });
}

test("Workflows asks the picked revision's application, not the one cached before", async ({ page }) => {
  await mockTwoApplications(page);
  const asked: URLSearchParams[] = [];
  page.on("request", (req) => {
    const url = new URL(req.url());
    if (url.pathname === "/api/developer/workflows") asked.push(url.searchParams);
  });
  await loadAs(page, "developer");

  // Workflows once in Getting started: the demo context is now cached.
  await page.getByRole("button", { name: "Workflows", exact: true }).click();
  await expect.poll(() => asked.some((p) => p.get("application_id") === "app-1")).toBe(true);

  // Budgeting's draft becomes the working revision, then straight to Workflows.
  await page.getByRole("button", { name: "Developer Models", exact: true }).click();
  const budgetRow = page.locator(".mvx-admin-revision", { hasText: BUDGET_REV.name });
  await budgetRow.click();
  await expect(budgetRow.getByText("Working")).toBeVisible({ timeout: 15_000 });
  asked.length = 0;
  await page.getByRole("button", { name: "Workflows", exact: true }).click();
  await expect.poll(() => asked.some((p) => p.get("revision_id") === BUDGET_REV.id)).toBe(true);
  for (const p of asked.filter((q) => q.get("revision_id") === BUDGET_REV.id)) {
    expect(p.get("application_id")).toBe("app-2");
  }
});
