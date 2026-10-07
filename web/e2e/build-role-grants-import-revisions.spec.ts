/**
 * Developer › Roles and the import wizard act on the working revision's model
 * (2026-09-29, several models in one application):
 *
 *  - Developer › Roles listed the working revision's dashboards but saved the
 *    grants with no revision, so the server replaced the grants among the
 *    selected model's LIVE dashboards: unticking a working-revision
 *    dashboard did nothing. Business Admin › Roles still sends none — it
 *    grants on the live revision of the selected model.
 *  - Integrations › New import filled its "Target revision" list with the
 *    revisions of whatever model the server resolved (X-Model-Id or the
 *    default), not the working revision's model — and its grid, form and
 *    dimension lists, the dimensions labels resolve against, the saved
 *    integrations and "Save as integration" all still came from that other
 *    model. They now all follow the working revision, which is also the
 *    default target revision.
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DEFAULT_REV = { id: "rev-default", name: "Tour live", created_at: "2026-09-01T00:00:00Z" };
const OTHER_LIVE = { id: "rev-other-live", name: "Sales live", created_at: "2026-09-01T00:00:00Z" };
const OTHER_DRAFT = { id: "rev-other-draft", name: "Sales draft", created_at: "2026-09-02T00:00:00Z" };

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

async function mockTwoModels(page: Page) {
  await mockApi(page);
  // Later registrations win over mockApi's catch-all.
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json([{
    id: "t-1", name: "Acme", plan: "test", created_at: "2026-01-01T00:00:00Z",
    applications: [{
      id: "app-1", name: "Getting started", mode: "planning", status: "active",
      models: [
        { id: "model-default", name: "Tour", storage_type: "oltp", active_revision: DEFAULT_REV.name, is_default: true, revisions: [DEFAULT_REV] },
        { id: "model-other", name: "Sales planning", storage_type: "oltp", active_revision: OTHER_LIVE.name, is_default: false, revisions: [OTHER_LIVE, OTHER_DRAFT] },
      ],
    }],
  }])));
  // A model named → its revisions; none → the server's default model.
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json(
    new URL(route.request().url()).searchParams.get("model_id") === "model-other"
      ? [{ ...OTHER_LIVE, is_active: true }, { ...OTHER_DRAFT, is_active: false }]
      : [{ ...DEFAULT_REV, is_active: true }],
  )));
  // The model a revision belongs to (Build's own query, and the wizard's).
  await page.route("**/api/developer/model*", (route) => {
    const rev = new URL(route.request().url()).searchParams.get("revision_id");
    const modelId = rev === OTHER_DRAFT.id || rev === OTHER_LIVE.id ? "model-other" : "model-default";
    return route.fulfill(json({ app_name: "Getting started", model_name: modelId, model_id: modelId, metrics: [] }));
  });
}

/** Picks Sales draft as the working revision on the Models tab. */
async function workInOtherDraft(page: Page) {
  await loadAs(page, "developer");
  const draftRow = page.locator(".mvx-admin-revision", { hasText: OTHER_DRAFT.name });
  await draftRow.click();
  await expect(draftRow.getByText("Working")).toBeVisible({ timeout: 15_000 });
}

/** Captures every PUT of a role's dashboard grants. */
async function capturePuts(page: Page): Promise<URL[]> {
  const puts: URL[] = [];
  await page.route("**/api/business-admin/roles/*/dashboards*", (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    puts.push(new URL(route.request().url()));
    return route.fulfill(json({ status: "ok" }));
  });
  return puts;
}

async function tickAndSave(page: Page, roleName: string, dashboardName: string) {
  const role = page.locator(".mvx-admin-object", { hasText: roleName }).first();
  await role.getByRole("button", { name: "Configure" }).click();
  await role.getByLabel(dashboardName).click();
  await page.getByRole("button", { name: "Save dashboard access" }).click();
}

test("Developer › Roles saves dashboard grants for the working revision", async ({ page }) => {
  await mockTwoModels(page);
  const puts = await capturePuts(page);
  await workInOtherDraft(page);
  await page.getByRole("button", { name: "Roles", exact: true }).click();

  await tickAndSave(page, "Finance Editors", "Sales Pipeline");
  await expect.poll(() => puts.length).toBe(1);
  expect(puts[0].pathname).toBe("/api/business-admin/roles/role-1/dashboards");
  expect(puts[0].searchParams.get("revision_id")).toBe(OTHER_DRAFT.id);
});

test("Business Admin › Roles saves grants with no revision (the live one)", async ({ page }) => {
  await mockApi(page);
  const puts = await capturePuts(page);
  await loadAs(page, "finance");
  await page.getByRole("button", { name: "Roles", exact: true }).click();

  await tickAndSave(page, "Finance Editors", "Sales Pipeline");
  await expect.poll(() => puts.length).toBe(1);
  expect(puts[0].pathname).toBe("/api/business-admin/roles/role-1/dashboards");
  expect(puts[0].searchParams.has("revision_id")).toBe(false);
});

test("the import wizard's target revisions are the working revision's model's", async ({ page }) => {
  await mockTwoModels(page);
  await workInOtherDraft(page);
  await page.getByRole("button", { name: "Integrations", exact: true }).click();
  await page.getByRole("button", { name: "New Import" }).click();

  const target = page.getByLabel("Target revision");
  await expect(target.locator("option", { hasText: OTHER_DRAFT.name })).toHaveCount(1, { timeout: 15_000 });
  await expect(target.locator("option", { hasText: `${OTHER_LIVE.name} (active)` })).toHaveCount(1);
  // The server's default model is not the one being built.
  await expect(target.locator("option", { hasText: DEFAULT_REV.name })).toHaveCount(0);
});

/** Grids, forms, dimensions and integrations per model; records creations. */
async function mockRevisionTargets(page: Page): Promise<URL[]> {
  const created: URL[] = [];
  const other = (route: { request(): { url(): string } }) =>
    [OTHER_LIVE.id, OTHER_DRAFT.id].includes(new URL(route.request().url()).searchParams.get("revision_id") ?? "");
  await page.route("**/api/developer/grids*", (route) => route.fulfill(json(other(route)
    ? [{ id: "grid-sales", name: "Sales Plan Grid", metric_ids: [], dimension_ids: ["dim-sales"] }]
    : [{ id: "grid-tour", name: "Tour Grid", metric_ids: [], dimension_ids: ["dim-tour"] }])));
  await page.route("**/api/forms*", (route) => route.fulfill(json(other(route)
    ? [{ id: "form-sales", model_id: "model-other", name: "deal", label: "Deal", fields: [], created_at: "2026-09-01T00:00:00Z" }]
    : [{ id: "form-tour", model_id: "model-default", name: "tour_request", label: "Tour request", fields: [], created_at: "2026-09-01T00:00:00Z" }])));
  await page.route("**/api/developer/dimensions*", (route) => route.fulfill(json(other(route)
    ? [{ id: "dim-sales", name: "Channel", agg_rule: "sum", members: [] }]
    : [{ id: "dim-tour", name: "Tour step", agg_rule: "sum", members: [] }])));
  await page.route("**/api/developer/integrations*", (route) => {
    if (route.request().method() === "POST") {
      created.push(new URL(route.request().url()));
      return route.fulfill(json({ id: "int-new" }));
    }
    return route.fulfill(json(other(route)
      ? [{ id: "int-sales", name: "Sales actuals", type: "csv_import", target_type: "grid", target_id: "grid-sales", status: "active", config: {} }]
      : [{ id: "int-tour", name: "Tour import", type: "csv_import", target_type: "grid", target_id: "grid-tour", status: "active", config: {} }]));
  });
  return created;
}

test("the import wizard's targets, saved integrations and new integration are the working revision's", async ({ page }) => {
  await mockTwoModels(page);
  const created = await mockRevisionTargets(page);
  await workInOtherDraft(page);
  await page.getByRole("button", { name: "Integrations", exact: true }).click();

  // Saved integrations: the working revision's, not the default model's.
  await expect(page.getByText("Sales actuals")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("Tour import")).toHaveCount(0);
  await page.getByRole("button", { name: "New Import" }).click();

  // The target revision starts as the working revision.
  const revision = page.getByLabel("Target revision");
  await expect(revision.locator("option", { hasText: OTHER_DRAFT.name })).toHaveCount(1, { timeout: 15_000 });
  await expect(revision).toHaveValue(OTHER_DRAFT.id);

  const targetType = page.getByLabel("Import target type");
  const optionsOf = (label: string) => page.getByLabel(label, { exact: true }).locator("option");
  await expect(optionsOf("Grid")).toHaveText(["— select —", "Sales Plan Grid"]);
  await targetType.selectOption("form");
  await expect(optionsOf("Form")).toHaveText(["— select —", "Deal"]);
  await targetType.selectOption("dimension");
  await expect(optionsOf("Dimension")).toHaveText(["— select —", "Channel"]);
  // Back to a grid: the working revision is the target again.
  await targetType.selectOption("grid");
  await expect(revision).toHaveValue(OTHER_DRAFT.id);

  // "Save as integration" creates it in the working revision.
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "Sales Plan Grid" });
  await page.locator('input[type="file"]').setInputFiles({ name: "sales.csv", mimeType: "text/csv", buffer: Buffer.from("Channel,Value\nWeb,10\n") });
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: "Next: Map Columns (no shaping)" }).click();
  await page.getByRole("button", { name: "Save as Integration" }).click();
  await page.getByPlaceholder("Integration name (e.g. Import OPEX Data)").fill("Sales CSV");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => created.length).toBe(1);
  expect(created[0].searchParams.get("revision_id")).toBe(OTHER_DRAFT.id);
});
