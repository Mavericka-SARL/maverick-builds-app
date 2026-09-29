/**
 * Workflows › Create Trigger makes a rule that fires on the workflow's own
 * start event, and only on it (2026-09-29). The dialog worked out the rule's
 * trigger type itself and knew only "form.submit" and "api.workflow.start":
 * a per-form event ("purchase_request.submitted") or a per-integration one
 * ("import_opex_data.import.completed" / ".import.failed") fell through to
 * "manual", with no workflow_def_id, so the rule never fired on the event.
 * Typed but sent with no source, the rule would have fired for EVERY form
 * submitted or every import (the dispatcher reads an empty source as "any").
 * Both the dialog and Build › Triggers now take the type and the one form or
 * integration from the working revision's trigger-event catalog, and the
 * workflow's Usage panel shows the new rule at once.
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const REV = { id: "rev-1", name: "FY2026 Budget", created_at: "2026-09-01T00:00:00Z", is_active: true };

const summary = (id: string, name: string, trigger_event: string) => ({
  id, application_id: "app-1", name, description: "", trigger_event,
  status: "published", step_count: 1, created_at: "2026-09-01T00:00:00Z", updated_at: "2026-09-01T00:00:00Z",
});

// form-1 and int-1 are the mocked API's form and integration.
const WORKFLOWS = [
  { wf: summary("wf-form", "Purchase approval", "purchase_request.submitted"), type: "form_submit", chip: "form_submit · Purchase Request", source: { source_form_id: "form-1" } },
  { wf: summary("wf-any-form", "Any request", "form.submit"), type: "form_submit", chip: "form_submit", source: {} },
  { wf: summary("wf-import-ok", "OPEX loaded", "import_opex_data.import.completed"), type: "integration_completed", chip: "integration_completed · Import OPEX Data", source: { source_integration_id: "int-1" } },
  { wf: summary("wf-import-fail", "OPEX failed", "import_opex_data.import.failed"), type: "integration_failed", chip: "integration_failed · Import OPEX Data", source: { source_integration_id: "int-1" } },
  { wf: summary("wf-api", "Started by API", "api.workflow.start"), type: "api", chip: "api", source: {} },
  { wf: summary("wf-manual", "Month-end close", "manual"), type: "manual", chip: "manual", source: {} },
  // Its form was renamed: the catalog no longer lists the event.
  { wf: summary("wf-stale", "Old approval", "old_request.submitted"), type: "form_submit", chip: "form_submit", source: {} },
];

const item = (key: string, category: string, source_type: string, source_id?: string, source_name?: string) => ({
  key, label: key, description: "", category, source_type, source_id, source_name, payload_schema: [], enabled: true, created_from: source_type,
});

// The working revision's trigger-event catalog.
const CATALOG = [
  item("manual", "manual", "system"),
  item("api.workflow.start", "api", "system"),
  item("form.submit", "form", "system"),
  item("purchase_request.submitted", "form", "form", "form-1", "Purchase Request"),
  item("integration.import.completed", "integration", "system"),
  item("import_opex_data.import.completed", "integration", "integration", "int-1", "Import OPEX Data"),
  item("import_opex_data.import.failed", "integration", "integration", "int-1", "Import OPEX Data"),
];

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

type Posted = { trigger_type: string; workflow_def_id?: string; workflow_name: string; name: string; source_form_id?: string; source_integration_id?: string };

async function mockWorkflows(page: Page): Promise<{ posted: Posted[]; catalogUrls: URL[] }> {
  const posted: Posted[] = [];
  const catalogUrls: URL[] = [];
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([REV])));
  await page.route("**/api/developer/workflows?*", (route) => route.fulfill(json(WORKFLOWS.map((w) => w.wf))));
  await page.route("**/api/developer/workflow-trigger-events*", (route) => {
    catalogUrls.push(new URL(route.request().url()));
    return route.fulfill(json(CATALOG));
  });
  for (const { wf } of WORKFLOWS) {
    await page.route(`**/api/developer/workflows/${wf.id}*`, (route) => route.fulfill(json({
      ...wf, subject_type: "", subject_config: {}, steps: [], context_schema: [], published_at: "2026-09-01T00:00:00Z", archived_at: null,
    })));
    await page.route(`**/api/developer/workflows/${wf.id}/instances*`, (route) => route.fulfill(json([])));
    // The rules that start this workflow: the ones created so far.
    await page.route(`**/api/developer/workflows/${wf.id}/usage*`, (route) => route.fulfill(json(
      posted.filter((p) => p.workflow_def_id === wf.id)
        .map((p, i) => ({ rule_id: `rule-${wf.id}-${i}`, rule_name: p.name, trigger_type: p.trigger_type, enabled: true })),
    )));
  }
  await page.route("**/api/automation/rules*", (route) => {
    const req = route.request();
    if (req.method() !== "POST") return route.fallback();
    const body = req.postDataJSON() as Posted;
    posted.push(body);
    return route.fulfill(json({ id: `rule-${posted.length}`, ...body, enabled: true, created_at: "2026-09-29T00:00:00Z" }));
  });
  return { posted, catalogUrls };
}

test("Create Trigger on a workflow row makes a rule for the workflow's own start event and source", async ({ page }) => {
  const { posted, catalogUrls } = await mockWorkflows(page);
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Workflows", exact: true }).click();

  const creatable = WORKFLOWS.filter((w) => w.wf.id !== "wf-stale");
  for (const [i, { wf, type, chip, source }] of creatable.entries()) {
    await page.locator("tr", { hasText: wf.name }).getByTitle("Actions").click();
    await page.getByRole("button", { name: "Create Trigger", exact: true }).click();
    // The dialog names the type, and the source, the rule will get.
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(`Workflow: ${wf.name}`)).toBeVisible();
    await expect(dialog.getByText(chip, { exact: true })).toBeVisible();
    await dialog.getByRole("button", { name: "Create Trigger", exact: true }).click();
    await expect.poll(() => posted.length).toBe(i + 1);
    expect(posted[i], wf.trigger_event).toMatchObject({ trigger_type: type, workflow_def_id: wf.id, workflow_name: wf.name, ...source });
    // No source for the events that name none: those fire for any form or import.
    if (!("source_form_id" in source)) expect(posted[i].source_form_id, wf.trigger_event).toBeUndefined();
    if (!("source_integration_id" in source)) expect(posted[i].source_integration_id, wf.trigger_event).toBeUndefined();
  }
  // The catalog is the working revision's.
  expect(catalogUrls.some((u) => u.searchParams.get("revision_id") === REV.id)).toBe(true);
});

test("Create Trigger refuses a per-form workflow whose form the catalog does not list", async ({ page }) => {
  const { posted } = await mockWorkflows(page);
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Workflows", exact: true }).click();

  await page.locator("tr", { hasText: "Old approval" }).getByTitle("Actions").click();
  await page.getByRole("button", { name: "Create Trigger", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("alert")).toContainText("old_request.submitted");
  await expect(dialog.getByRole("button", { name: "Create Trigger", exact: true })).toBeDisabled();
  expect(posted).toHaveLength(0);
});

test("Build › Triggers fills in the form or integration a per-source workflow starts on", async ({ page }) => {
  const { posted } = await mockWorkflows(page);
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Triggers", exact: true }).click();
  await page.getByRole("button", { name: "New rule" }).click();

  const workflow = page.getByLabel("Workflow", { exact: true });
  await expect(workflow.locator("option", { hasText: "Purchase approval" })).toHaveCount(1, { timeout: 15_000 });
  await workflow.selectOption({ label: "Purchase approval (1 steps)" });
  await expect(page.getByLabel("Source form")).toHaveValue("form-1");

  await workflow.selectOption({ label: "OPEX loaded (1 steps)" });
  await expect(page.getByLabel("Source integration")).toHaveValue("int-1");

  await page.getByPlaceholder("rule_name").fill("opex_loaded");
  await page.getByRole("button", { name: "Create rule" }).click();
  await expect.poll(() => posted.length).toBe(1);
  expect(posted[0]).toMatchObject({ trigger_type: "integration_completed", workflow_def_id: "wf-import-ok", source_integration_id: "int-1", source_form_id: "" });
});

test("a trigger created from the Usage panel shows there at once", async ({ page }) => {
  const { posted } = await mockWorkflows(page);
  const wf = WORKFLOWS[0].wf;
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Workflows", exact: true }).click();
  await page.getByRole("button", { name: wf.name, exact: true }).first().click();

  await page.getByRole("button", { name: /No rule yet/ }).click();
  await page.getByRole("button", { name: "+ Create Trigger", exact: true }).click();
  await page.getByRole("button", { name: "Create Trigger", exact: true }).click();
  await expect.poll(() => posted.length).toBe(1);
  expect(posted[0]).toMatchObject({ trigger_type: "form_submit", workflow_def_id: wf.id, source_form_id: "form-1" });

  const ruleName = `${wf.name} — Trigger`;
  await expect(page.getByText(`⚡ ${ruleName}`)).toBeVisible();
  await expect(page.getByText("form_submit · Enabled")).toBeVisible();
});
