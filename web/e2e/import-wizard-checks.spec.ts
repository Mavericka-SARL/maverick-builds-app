/**
 * What the Episode 4 try-out of the Import Wizard found (2026-10-21), in the
 * console:
 * - a workbook with an empty row under its title came back from the Shape
 *   step's preview with that row as null, and Preview blanked the console;
 * - a file integration saved with "Replace listed cells" kept no mode and
 *   ran as incremental, and nothing showed or set its mode again;
 * - Recent Imports showed no revision, and nothing of a refused import.
 *
 * The server side is internal/gateway/import_wizard_checks_test.go.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

async function openIntegrations(page: Page) {
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
}

async function chooseGridFile(page: Page, file: string) {
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "OPEX Planning Grid" });
  await page.getByLabel("Target revision").selectOption("rev-1");
  await page.locator('input[type="file"]').setInputFiles({ name: "plan.csv", mimeType: "text/csv", buffer: Buffer.from(file) });
}

test.beforeEach(async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
});

test("an empty row under a workbook's title previews as an empty row", async ({ page }) => {
  await page.route("**/api/import/reshape-preview", (route) => route.fulfill(json({
    raw: [["Plan FY26"], null, ["Department", "Region", "opex_marketing"], ["SALES", "EMEA", "10"]],
    header: ["Department", "Region", "opex_marketing"], rows: [["SALES", "EMEA", "10"]], lines: [4], row_count: 1,
  })));
  await openIntegrations(page);
  await page.getByRole("button", { name: "New Import" }).click();
  await chooseGridFile(page, "Plan FY26\n\nDepartment,Region,opex_marketing\nSALES,EMEA,10\n");
  await page.getByRole("button", { name: "Next: Shape" }).click();
  const shape = page.getByTestId("shape-step");
  await shape.getByLabel("Header row").fill("3");
  await shape.getByRole("button", { name: "Preview" }).click();

  await expect(page.getByTestId("shape-preview")).toContainText("Department");
  await expect(shape.getByRole("cell", { name: "Plan FY26" })).toBeVisible();
  await page.getByRole("button", { name: "Next: Map Columns", exact: true }).click();
  await expect(page.getByLabel("Map opex_marketing to")).toHaveValue("opex_marketing");
});

test("a file integration keeps the import mode it was saved with, shows it, and its edit keeps it", async ({ page }) => {
  const configs: Array<Record<string, unknown>> = [];
  await page.route((url) => /\/api\/developer\/integrations\/[^/]+\/config$/.test(url.pathname), async (route) => {
    configs.push(route.request().postDataJSON().config);
    await route.fulfill(json({ status: "updated" }));
  });
  await page.route((url) => url.pathname === "/api/developer/integrations" || url.pathname === "/api/integrations", (route) =>
    route.request().method() === "POST" ? route.fulfill(json({ id: "int-new" })) : route.fulfill(json([{
      id: "int-m", name: "Monthly sales actuals", type: "csv_import", target_type: "grid", target_id: "grid-1", status: "active",
      config: { column_map: { Department: "Department", Region: "Region", opex_marketing: "opex_marketing" }, import_mode: "replace" },
    }])));
  await openIntegrations(page);

  // The saved integration's line names its mode.
  await expect(page.getByTestId("integration-mode-int-m")).toHaveText("Mode: Replace listed cells");

  // Saved from the wizard after choosing Replace listed cells, a file
  // integration keeps it: only a Google Sheet's used to.
  await page.getByRole("button", { name: "New Import" }).click();
  await page.getByLabel(/Replace listed cells/).check();
  await chooseGridFile(page, "Department,Region,opex_marketing\nSALES,EMEA,10\n");
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: /^Next: Map Columns/ }).click();
  await page.getByRole("button", { name: "Save as Integration" }).click();
  await page.getByPlaceholder("Integration name (e.g. Import OPEX Data)").fill("Monthly sales actuals 2");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => configs.length).toBe(1);
  expect(configs[0]).toMatchObject({ import_mode: "replace" });
  await page.getByRole("button", { name: "Close import wizard" }).click();

  // Edit shape and mapping opens on the saved mode, and saves the one chosen.
  await page.getByRole("button", { name: "Edit shape and mapping of Monthly sales actuals" }).click();
  await expect(page.getByLabel(/Replace listed cells/)).toBeChecked();
  await page.getByLabel(/Full reload/).check();
  await chooseGridFile(page, "Department,Region,opex_marketing\nSALES,EMEA,10\n");
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: /^Next: Map Columns/ }).click();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect.poll(() => configs.length).toBe(2);
  expect(configs[1]).toMatchObject({ import_mode: "full_reload" });
});

test("Recent Imports names each import's revision and why a refused one went in not at all", async ({ page }) => {
  await page.route("**/api/import/jobs", (route) => route.fulfill(json([
    { id: "job-2", model_id: "model-1", revision_id: "rev-1", revision_name: "Working", status: 3, total_rows: 24, valid_rows: 24, error_rows: 0, created_at: "2026-10-21T10:00:00Z" },
    { id: "job-1", model_id: "model-1", revision_id: "rev-1", revision_name: "Working", status: 4, total_rows: 4, valid_rows: 0, error_rows: 4, created_at: "2026-10-21T09:00:00Z",
      error: "row 5, product: \"AI accel. cards\" is not a member of dimension \"product\"" },
  ])));
  await openIntegrations(page);
  const history = page.getByRole("table").filter({ has: page.getByRole("columnheader", { name: "Values" }) });
  await expect(history.getByRole("row")).toHaveCount(3);
  await expect(history.getByRole("row").nth(1)).toContainText("Working");
  await expect(history.getByRole("row").nth(2)).toContainText("Failed");
  await expect(history.getByRole("row").nth(2)).toContainText("row 5, product: \"AI accel. cards\" is not a member");
});
