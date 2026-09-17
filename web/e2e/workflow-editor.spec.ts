/**
 * Workflow constructor regressions (click-through audit, 2026-09-10). Mocked
 * API — CI runs without a gateway. Covers the UI-side fixes; the server-side
 * ones (steps-only PATCH keeping name/trigger, validate-by-draft, notification
 * recipient check, restore from archive) are Go tests in internal/gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const APP = "app-1";
const REV = "rev-1";
const WF_ID = "wf-audit";
const wf = {
  id: WF_ID, application_id: APP, name: "Audit flow", description: "", trigger_event: "manual",
  subject_type: "", subject_config: {}, status: "draft", steps: [], context_schema: [],
  created_at: "2026-09-01T00:00:00Z", updated_at: "2026-09-01T00:00:00Z", published_at: null, archived_at: null,
};

async function mockEditor(page: Page, log: string[]) {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/developer/workflows?*", (route) => route.fulfill(json([wf])));
  await page.route(`**/api/developer/workflows/${WF_ID}`, (route) => {
    log.push(`${route.request().method()} ${new URL(route.request().url()).pathname} ${route.request().postData() ?? ""}`);
    return route.fulfill(json(wf));
  });
  await page.route(`**/api/developer/workflows/${WF_ID}/usage`, (route) => route.fulfill(json([])));
  await page.route(`**/api/developer/workflows/${WF_ID}/instances`, (route) => route.fulfill(json([
    { id: "i-1", rule_id: "", application_id: APP, status: "completed", trigger_payload: {}, started_at: "2026-09-13T10:00:00Z", context: { dept: "SALES" } },
    { id: "i-2", rule_id: "", application_id: APP, status: "running", trigger_payload: {}, started_at: "2026-09-13T11:00:00Z", context: { dept: "ENG" }, test_run: true },
  ])));
  await page.route(`**/api/developer/workflows/${WF_ID}/validate`, (route) => {
    log.push(`POST validate ${route.request().postData() ?? "(no body)"}`);
    return route.fulfill(json({ valid: false, errors: ["Task step \"New Task\" has no assignee role"] }));
  });
  await page.route("**/api/developer/grids*", (route) => {
    log.push(`GET ${route.request().url().replace(/^.*\/api/, "/api")}`);
    return route.fulfill(json([{ id: "grid-1", name: "OPEX Planning Grid", revision_id: REV, metric_ids: ["m2"], dimension_ids: ["d1"] }]));
  });
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: REV, name: "Working", is_active: true }])));
}

async function openEditor(page: Page) {
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Workflows", exact: true }).click();
  await page.getByRole("button", { name: "Audit flow", exact: true }).first().click();
  await expect(page.getByText("Steps", { exact: true })).toBeVisible({ timeout: 15_000 });
}

test("object picker asks for THIS revision's grids", async ({ page }) => {
  const log: string[] = [];
  await mockEditor(page, log);
  await openEditor(page);
  await page.locator("select").filter({ has: page.locator('option[value="grid_metric"]') }).first().selectOption("grid");
  await expect.poll(() => log.filter((l) => l.startsWith("GET /api/developer/grids")).length).toBeGreaterThan(0);
  const req = log.find((l) => l.startsWith("GET /api/developer/grids"))!;
  expect(req).toContain(`revision_id=${REV}`);
});

test("Validate checks the unsaved draft and never saves it", async ({ page }) => {
  const log: string[] = [];
  await mockEditor(page, log);
  await openEditor(page);
  await page.getByRole("button", { name: /^Task$/ }).click();
  await expect(page.getByText("Unsaved changes")).toBeVisible();
  await page.getByRole("button", { name: "Validate", exact: true }).click();
  await expect(page.getByText("Validation Results")).toBeVisible({ timeout: 10_000 });
  const validate = log.find((l) => l.startsWith("POST validate"))!;
  expect(validate).toContain("New Task");
  expect(log.some((l) => l.startsWith("PATCH"))).toBe(false);
  await expect(page.getByText("Unsaved changes")).toBeVisible();
});

test("Test Run is disabled while there are unsaved changes", async ({ page }) => {
  const log: string[] = [];
  await mockEditor(page, log);
  await openEditor(page);
  const testRun = page.getByRole("button", { name: "Test Run", exact: true });
  await expect(testRun).toBeEnabled();
  await page.getByRole("button", { name: /^Task$/ }).click();
  await expect(testRun).toBeDisabled();
  await expect(testRun).toHaveAttribute("title", /Save first/);
});

test("a duplicate context variable key is refused with a hint", async ({ page }) => {
  const log: string[] = [];
  await mockEditor(page, log);
  await openEditor(page);
  await page.getByRole("button", { name: "context", exact: true }).click();
  await page.getByPlaceholder("variable_key").fill("country");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await page.getByPlaceholder("variable_key").fill("country");
  await expect(page.getByRole("button", { name: "Add", exact: true })).toBeDisabled();
  await expect(page.getByRole("alert").filter({ hasText: /already exists/ })).toBeVisible();
  await expect(page.locator('button[title="Remove"]')).toHaveCount(1);
});

test("leaving with unsaved changes asks through the design-system dialog", async ({ page }) => {
  const log: string[] = [];
  await mockEditor(page, log);
  await openEditor(page);
  let native = false;
  page.once("dialog", async (d) => { native = true; await d.dismiss(); });
  await page.getByRole("button", { name: /^Task$/ }).click();
  await page.getByRole("button", { name: /← Workflows/ }).click();
  await expect(page.getByRole("button", { name: "Discard changes", exact: true })).toBeVisible({ timeout: 5_000 });
  expect(native).toBe(false);
  await page.getByRole("button", { name: "Discard changes", exact: true }).click();
  await expect(page.getByRole("button", { name: "Audit flow", exact: true }).first()).toBeVisible({ timeout: 10_000 });
});

test("Instances section lists real runs with test runs marked; one-active-per-scope is a saved property", async ({ page }) => {
  const log: string[] = [];
  await mockEditor(page, log);
  await openEditor(page);

  // Developers had no way to see a definition's instances in their console.
  await page.getByRole("button", { name: "instances", exact: true }).click();
  const list = page.getByLabel("Workflow instances");
  await expect(list).toBeVisible({ timeout: 15_000 });
  await expect(list.getByText("completed", { exact: true })).toHaveCount(1);
  await expect(list.getByText("test run", { exact: true })).toHaveCount(1);
  await expect(list.getByText("dept: SALES")).toBeVisible();

  // Dedup is a per-definition choice: on by default, saved through PATCH.
  await page.getByRole("button", { name: "properties", exact: true }).click();
  const flag = page.getByLabel("One active instance per scope");
  await expect(flag).toBeChecked();
  await flag.uncheck();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => log.some((l) => l.startsWith("PATCH") && l.includes('"single_active_instance":false')), { timeout: 10_000 }).toBe(true);
});

test("editor follow-ups: one name field, rule badge, trigger fields in Context, kept object, delete says why", async ({ page }) => {
  const log: string[] = [];
  await mockEditor(page, log);
  // A second, published workflow for the list's Delete affordance.
  await page.route("**/api/developer/workflows?*", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([
    wf, { ...wf, id: "wf-live", name: "Live flow", status: "published", published_at: "2026-09-02T00:00:00Z" },
  ]) }));
  await page.route("**/api/developer/workflow-trigger-events*", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([
    { key: "manual", label: "Manual", description: "Started manually.", category: "manual", source_type: "system", payload_schema: [{ key: "started_by_user_id", type: "user", required: true }], enabled: true, created_from: "system" },
  ]) }));
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Workflows", exact: true }).click();
  await expect(page.getByRole("button", { name: "Live flow", exact: true }).first()).toBeVisible({ timeout: 15_000 });

  // Delete is listed but disabled with the reason on a published workflow.
  const liveRow = page.getByRole("row").filter({ hasText: "Live flow" });
  await liveRow.getByTitle("Actions").click();
  const del = page.getByRole("button", { name: "Delete", exact: true });
  await expect(del).toBeDisabled();
  await expect(del).toHaveAttribute("title", /Only a draft can be deleted/);
  await page.keyboard.press("Escape");

  await page.getByRole("button", { name: "Audit flow", exact: true }).first().click();
  await expect(page.getByText("Steps", { exact: true })).toBeVisible({ timeout: 15_000 });
  // One name field: the toolbar shows the name as text, Properties edits it.
  await expect(page.getByRole("textbox").filter({ hasNot: page.locator("[placeholder]") }).filter({ has: page.locator(':scope') }).locator('xpath=self::input[@value="Audit flow"]')).toHaveCount(1);
  // The badge reports rules, not the event.
  await expect(page.getByText("No rule yet")).toBeVisible();
  await expect(page.getByText("No trigger", { exact: true })).toHaveCount(0);
  // "Starts on" replaces "Trigger event"; the payload fields live in Context.
  await expect(page.getByText("Starts on", { exact: true })).toBeVisible();
  await expect(page.getByText("Expected context fields")).toHaveCount(0);
  await page.getByRole("button", { name: "context", exact: true }).click();
  await expect(page.getByLabel("Provided by the trigger")).toBeVisible();
  await expect(page.getByLabel("Provided by the trigger")).toContainText("started_by_user_id");

  // Switching grid → grid_metric keeps the chosen grid.
  await page.getByRole("button", { name: "properties", exact: true }).click();
  const objectType = page.locator("select").filter({ has: page.locator('option[value="grid_metric"]') }).first();
  await objectType.selectOption("grid");
  const gridSelect = page.locator("select").filter({ has: page.locator('option[value="grid-1"]') }).first();
  await gridSelect.selectOption("grid-1");
  await objectType.selectOption("grid_metric");
  await expect(page.locator("select").filter({ has: page.locator('option[value="grid-1"]') }).first()).toHaveValue("grid-1");
});
