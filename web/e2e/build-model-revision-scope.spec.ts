/**
 * Build acts on the model and revision the developer chose, not on the
 * application's default model (2026-09-29, new tenants get four models in
 * one application):
 *
 *  - Models › "New revision" on a model's row POSTed /api/developer/revisions
 *    without a model, so the server created the revision in the DEFAULT
 *    model — the row the developer acted on never got it.
 *  - Triggers listed rules, workflows, forms, grids and integrations with no
 *    revision and created rules with none, so the server answered with the
 *    default model's live revision whatever revision was being built. The
 *    same held for "Create Trigger" on a workflow and for the dashboard
 *    designer's form, rule and integration pickers.
 *  - Build's first working revision depended on request timing: when the
 *    model list answered before the default model's revisions, the newest
 *    revision of ANY model was picked.
 *  - The metric recalc banner read a field the server does not send.
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page, type Request } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DEFAULT_REV = { id: "rev-default", name: "Tour live", created_at: "2026-09-01T00:00:00Z" };
const OTHER_LIVE = { id: "rev-other-live", name: "Sales live", created_at: "2026-09-01T00:00:00Z" };
const OTHER_DRAFT = { id: "rev-other-draft", name: "Sales draft", created_at: "2026-09-02T00:00:00Z" };

const manualWf = {
  id: "wf-manual", application_id: "app-1", name: "Month-end close", description: "", trigger_event: "manual",
  status: "published", step_count: 2, created_at: "2026-09-01T00:00:00Z", updated_at: "2026-09-01T00:00:00Z",
};

// The full definition the editor opens (the list above is its summary).
const manualWfDef = {
  ...manualWf, subject_type: "", subject_config: {}, steps: [], context_schema: [], published_at: "2026-09-01T00:00:00Z", archived_at: null,
};

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
}

function revisionOf(req: Request): string | null {
  return new URL(req.url()).searchParams.get("revision_id");
}

test("New revision on a non-default model's row creates it in that model", async ({ page }) => {
  let postedUrl: URL | null = null;
  let postedBody: Record<string, unknown> | null = null;
  await mockTwoModels(page);
  await page.route("**/api/developer/revisions*", (route) => {
    const req = route.request();
    if (req.method() === "POST") {
      postedUrl = new URL(req.url());
      postedBody = req.postDataJSON();
      return route.fulfill(json({ id: "rev-new" }));
    }
    // No model named → the server's default model.
    const model = new URL(req.url()).searchParams.get("model_id");
    if (model === "model-other") {
      return route.fulfill(json([{ ...OTHER_LIVE, is_active: true }, { ...OTHER_DRAFT, is_active: false }]));
    }
    // The default model's revisions answer AFTER the model list: the order
    // that used to make Build open in the newest revision of any model
    // (Sales draft) instead of the default model's live one.
    return new Promise<void>((resolve) => setTimeout(() => resolve(route.fulfill(json([{ ...DEFAULT_REV, is_active: true }]))), 1_000));
  });

  await loadAs(page, "developer");
  const otherModel = page.locator(".mvx-admin-model", { hasText: "Sales planning" });
  await expect(otherModel).toBeVisible({ timeout: 15_000 });
  await expect(page.locator(".mvx-context-banner")).toContainText(`Working in revision: ${DEFAULT_REV.name}`);
  await otherModel.getByRole("button", { name: "New revision" }).click();
  await otherModel.getByPlaceholder("Revision name…").fill("Sales what-if");
  await otherModel.getByRole("button", { name: "Save" }).click();

  await expect.poll(() => postedUrl?.searchParams.get("model_id") ?? null).toBe("model-other");
  // The copy source is that model's own live revision (the working revision,
  // asserted above, belongs to the default model, so it is not a candidate).
  expect(postedBody).toMatchObject({ name: "Sales what-if", source_revision_id: OTHER_LIVE.id });
});

test("Triggers lists and creates in the working revision, and follows a revision switch", async ({ page }) => {
  const seen: Record<string, (string | null)[]> = { rules: [], workflows: [], forms: [], grids: [], integrations: [] };
  let postedRuleUrl: URL | null = null;
  await mockTwoModels(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...DEFAULT_REV, is_active: true }])));
  await page.route("**/api/developer/workflows?*", (route) => {
    seen.workflows.push(revisionOf(route.request()));
    return route.fulfill(json([manualWf]));
  });
  await page.route("**/api/automation/rules*", (route) => {
    const req = route.request();
    if (req.method() === "POST") {
      postedRuleUrl = new URL(req.url());
      return route.fulfill(json({ id: "rule-new", ...req.postDataJSON(), enabled: true, created_at: "2026-09-29T00:00:00Z" }));
    }
    seen.rules.push(revisionOf(req));
    return route.fallback();
  });
  for (const [key, pattern] of [["forms", "**/api/forms*"], ["grids", "**/api/developer/grids*"], ["integrations", "**/api/integrations*"]] as const) {
    await page.route(pattern, (route) => {
      if (route.request().method() === "GET" && new URL(route.request().url()).pathname.split("/").length <= 4) {
        seen[key].push(revisionOf(route.request()));
      }
      return route.fallback();
    });
  }

  await loadAs(page, "developer");
  const draftRow = page.locator(".mvx-admin-revision", { hasText: OTHER_DRAFT.name });
  await draftRow.click();
  await expect(draftRow.getByText("Working")).toBeVisible({ timeout: 15_000 });
  for (const key of Object.keys(seen)) seen[key].length = 0;

  await page.getByRole("button", { name: "Triggers", exact: true }).click();
  for (const key of Object.keys(seen)) {
    await expect.poll(() => seen[key], { message: `${key} request carries the working revision` }).toContain(OTHER_DRAFT.id);
    // Nothing on this tab asked without a revision (the default model's live one).
    expect(seen[key], `${key} requests`).not.toContain(null);
  }

  await page.getByRole("button", { name: "New rule" }).click();
  await page.getByPlaceholder("rule_name").fill("close_sales");
  await page.locator(".mvx-panel select").nth(0).selectOption("wf-manual");
  await page.getByRole("button", { name: "Create rule" }).click();
  await expect.poll(() => postedRuleUrl?.searchParams.get("revision_id") ?? null).toBe(OTHER_DRAFT.id);

  // Switching the working revision refetches the tab for the new one.
  await page.getByRole("button", { name: "Models", exact: true }).first().click();
  const liveRow = page.locator(".mvx-admin-revision", { hasText: OTHER_LIVE.name });
  await liveRow.click();
  await expect(liveRow.getByText("Working")).toBeVisible();
  await page.getByRole("button", { name: "Triggers", exact: true }).click();
  await expect.poll(() => seen.rules).toContain(OTHER_LIVE.id);
});

/** Picks Sales draft as the working revision on the Models tab. */
async function workInOtherDraft(page: Page) {
  await loadAs(page, "developer");
  const draftRow = page.locator(".mvx-admin-revision", { hasText: OTHER_DRAFT.name });
  await draftRow.click();
  await expect(draftRow.getByText("Working")).toBeVisible({ timeout: 15_000 });
}

test("Create Trigger on a workflow row and in Usage creates the rule in the working revision", async ({ page }) => {
  const posted: (string | null)[] = [];
  await mockTwoModels(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...DEFAULT_REV, is_active: true }])));
  await page.route("**/api/developer/workflows?*", (route) => route.fulfill(json([manualWf])));
  await page.route(`**/api/developer/workflows/${manualWf.id}*`, (route) => route.fulfill(json(manualWfDef)));
  await page.route(`**/api/developer/workflows/${manualWf.id}/usage*`, (route) => route.fulfill(json([])));
  await page.route(`**/api/developer/workflows/${manualWf.id}/instances*`, (route) => route.fulfill(json([])));
  await page.route("**/api/automation/rules*", (route) => {
    const req = route.request();
    if (req.method() !== "POST") return route.fallback();
    posted.push(revisionOf(req));
    return route.fulfill(json({ id: `rule-${posted.length}`, ...req.postDataJSON(), enabled: true, created_at: "2026-09-29T00:00:00Z" }));
  });

  await workInOtherDraft(page);
  await page.getByRole("button", { name: "Workflows", exact: true }).click();

  // The row's actions menu.
  await page.getByTitle("Actions").first().click();
  await page.getByRole("button", { name: "Create Trigger", exact: true }).click();
  await page.getByRole("button", { name: "Create Trigger", exact: true }).click();
  await expect.poll(() => posted).toEqual([OTHER_DRAFT.id]);

  // The editor's Usage panel.
  await page.getByRole("button", { name: manualWf.name, exact: true }).first().click();
  await page.getByRole("button", { name: /No rule yet/ }).click();
  await page.getByRole("button", { name: "+ Create Trigger", exact: true }).click();
  await page.getByRole("button", { name: "Create Trigger", exact: true }).click();
  await expect.poll(() => posted).toEqual([OTHER_DRAFT.id, OTHER_DRAFT.id]);
});

test("the dashboard designer's form, rule and integration pickers list the dashboard's revision", async ({ page }) => {
  const seen: Record<string, (string | null)[]> = { "/api/forms": [], "/api/automation/rules": [], "/api/developer/integrations": [] };
  await mockTwoModels(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...DEFAULT_REV, is_active: true }])));
  await page.route("**/api/developer/folders*", (route) => route.fulfill(json([])));
  await page.route("**/api/developer/dashboards*", (route) => route.fulfill(json(revisionOf(route.request()) === OTHER_DRAFT.id ? [{
    id: "dash-sales", name: "Sales cockpit", tags: [], folder_id: null,
    widgets: [{
      id: "w-1", widget_type: "text", ref_id: null, content: "SALES-DRAFT-WIDGET", title: null, show_title: false,
      widget_props: null, sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 300, size_h: 60,
    }],
  }] : [])));

  await workInOtherDraft(page);
  await page.getByRole("button", { name: "Dashboards", exact: true }).click();
  const row = page.getByRole("button", { name: "Sales cockpit", exact: true }).locator("xpath=ancestor::div[contains(@class,'mvx-admin-object')][1]");
  await expect(row).toBeVisible({ timeout: 15_000 });
  // Nothing on the Dashboards list asks for these; the designer does.
  page.on("request", (req) => {
    const path = new URL(req.url()).pathname;
    if (req.method() === "GET" && path in seen) seen[path].push(revisionOf(req));
  });
  await row.getByRole("button", { name: "Design", exact: true }).click();
  await expect(page.getByText("SALES-DRAFT-WIDGET").first()).toBeVisible({ timeout: 15_000 });

  for (const path of Object.keys(seen)) {
    await expect.poll(() => seen[path], { message: `${path} asked for the dashboard's revision` }).toContain(OTHER_DRAFT.id);
    expect(seen[path], `${path} requests`).not.toContain(null);
  }
});

test("the metric recalc banner names each revision, or shows a short id when it has no name", async ({ page }) => {
  const UNNAMED = "0123456789abcdef";
  // Only the metric's own model knows this one: the names come from THAT
  // model's revisions, not the default model's.
  const WHAT_IF = { id: "rev-metric-what-if", name: "What-if", created_at: "2026-09-03T00:00:00Z" };
  await mockTwoModels(page);
  // The Metrics tab's model (mocks.ts) is model-1.
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json(
    new URL(route.request().url()).searchParams.get("model_id") === "model-1"
      ? [{ ...DEFAULT_REV, is_active: true }, { ...WHAT_IF, is_active: false }]
      : [{ ...DEFAULT_REV, is_active: true }],
  )));
  await page.route("**/api/developer/metrics/m1*", (route) => route.request().method() === "PATCH"
    ? route.fulfill(json({ status: "ok", recalc: [
      { revision_id: DEFAULT_REV.id, metric: "revenue", value: 42 },
      { revision_id: WHAT_IF.id, metric: "revenue", value: 7 },
      { revision_id: UNNAMED, metric: "revenue", value: null },
    ] }))
    : route.fallback());

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Metrics", exact: true }).click();
  await page.getByRole("button", { name: "Edit metric revenue" }).click();
  await page.getByRole("button", { name: "Save", exact: true }).click();

  const banner = page.locator("tr", { hasText: "Recalc results" });
  await expect(banner).toContainText(`${DEFAULT_REV.name} → revenue = 42`);
  await expect(banner).toContainText(`${WHAT_IF.name} → revenue = 7`);
  await expect(banner).toContainText(`${UNNAMED.slice(0, 8)} → revenue = —`);
});
