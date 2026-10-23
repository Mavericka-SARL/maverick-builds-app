/**
 * The Import Wizard sends each target's file where that target is imported,
 * in the layouts the rest of the product uses (reported by a self-hosting
 * tester, 2026-10-19):
 * - a parent_code column was auto-mapped onto code ("parentcode" contains
 *   "code"), so a dimension import relabelled members and never made the
 *   new ones;
 * - a grid file with one column per metric — the grid export's and the
 *   dashboard import's layout — could not be mapped at all;
 * - records went to the grid importer and were refused;
 * - a refused file said only how many rows failed, not why;
 * - the Dimensions page kept the members from before the import.
 * And from the Episode 4 try-out (2026-10-21): Validate passed a row naming
 * a member the model does not have, which the commit then refused, and the
 * refusal offered only Done.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

async function openWizard(page: Page, targetType: string, label: string, target: string, file: string) {
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "New Import" }).click();
  await page.getByLabel("Import target type").selectOption(targetType);
  await page.getByLabel(label, { exact: true }).selectOption({ label: target });
  if (targetType === "grid") await page.getByLabel("Target revision").selectOption("rev-1");
  await page.locator('input[type="file"]').setInputFiles({ name: "file.csv", mimeType: "text/csv", buffer: Buffer.from(file) });
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: /^Next: Map Columns/ }).click();
}

async function commit(page: Page) {
  await page.getByRole("button", { name: "Next: Validate" }).click();
  await page.getByRole("button", { name: /Commit Import/ }).first().click();
  await page.getByRole("button", { name: /Commit Import \(\d+ rows?\)/ }).click();
}

test.beforeEach(async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
});

test("a parent_code column maps to the parent, and the imported members show without a reload", async ({ page }) => {
  const sent: string[] = [];
  await page.route("**/api/import/dimension-members", async (route) => {
    sent.push(route.request().postDataJSON().csv);
    await route.fulfill(json({ rows_imported: 2, error_rows: 0 }));
  });
  await openWizard(page, "dimension", "Dimension", "Department", "code,label,parent_code\nFIN,Finance,OPEX\nHR,People,OPEX\n");

  await expect(page.getByLabel("Map parent_code to")).toHaveValue("parent_code");
  await expect(page.getByLabel("Map code to")).toHaveValue("code");
  let reads = 0;
  page.on("request", (r) => { if (new URL(r.url()).pathname === "/api/developer/dimensions") reads++; });
  await commit(page);

  await expect(page.getByText("Import Complete")).toBeVisible();
  expect(sent[0].split("\n")[0]).toBe("code,label,parent_code");
  await expect.poll(() => reads).toBeGreaterThan(0);
});

test("two columns mapped to one target are refused before the import", async ({ page }) => {
  await openWizard(page, "dimension", "Dimension", "Department", "code,label,parent_code\nFIN,Finance,OPEX\n");
  await page.getByLabel("Map parent_code to").selectOption("code");
  await expect(page.getByText(/Columns "code" and "parent_code" both map to code/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Next: Validate" })).toBeDisabled();
});

test("a grid file with one column per metric maps and imports as it is", async ({ page }) => {
  const sent: string[] = [];
  await page.route("**/api/import/upload", async (route) => {
    const body = route.request().postDataJSON();
    if (body.dry_run) return route.fulfill(json({ dry_run: true, rows: 1, values: 2, error_rows: 0, errors: [] }));
    sent.push(body.csv);
    await route.fulfill(json({ job_id: "job-1", status: "committed", total_rows: 2, valid_rows: 2, error_rows: 0, revision_id: "rev-1", rows: 1, values: 2 }));
  });
  await openWizard(page, "grid", "Grid", "OPEX Planning Grid", "Department,Region,opex_marketing,People OPEX\nSALES,EMEA,10,20\n");

  // By name or by label; a calculated metric of the grid is no target.
  await expect(page.getByLabel("Map opex_marketing to")).toHaveValue("opex_marketing");
  await expect(page.getByLabel("Map People OPEX to")).toHaveValue("opex_people");
  await expect(page.getByLabel("Map Department to").locator("option", { hasText: "Total OPEX" })).toHaveCount(0);
  await expect(page.getByText("Still missing")).toHaveCount(0);
  await commit(page);

  await expect.poll(() => sent.length).toBe(1);
  expect(sent[0]).toBe("Department,Region,opex_marketing,opex_people\nSALES,EMEA,10,20");
  // One row of two metrics: one row, two values — it read "2 rows imported".
  await expect(page.getByText("1 rows imported (2 values)")).toBeVisible();
});

test("a long grid file names each row's metric, sent by name; Validate lists the rows the import would refuse", async ({ page }) => {
  const sent: Array<{ csv: string; dry_run?: boolean; row_lines?: number[] }> = [];
  // The server names a row by the line the wizard says it came from.
  const mars = { row: 5, column: "Region", code: "UNKNOWN_MEMBER", message: "\"MARS\" is not a member of dimension \"Region\"", raw_value: "MARS" };
  await page.route("**/api/import/upload", async (route) => {
    const body = route.request().postDataJSON();
    sent.push(body);
    if (body.dry_run) return route.fulfill(json({ dry_run: true, rows: 1, values: 1, error_rows: 1, errors: [mars] }));
    await route.fulfill(json({ error: "import rejected: 1 row(s) failed validation, nothing was imported", error_rows: 1, errors: [mars] }, 422));
  });
  // A title and a blank line above the header on line 3: the file's rows
  // start on line 4.
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "New Import" }).click();
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "OPEX Planning Grid" });
  await page.getByLabel("Target revision").selectOption("rev-1");
  await page.locator('input[type="file"]').setInputFiles({ name: "file.csv", mimeType: "text/csv",
    buffer: Buffer.from("OPEX actuals\n\nDepartment,Region,metric,value\nSALES,EMEA,Marketing OPEX,5\nSALES,MARS,Marketing OPEX,10\n") });
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByTestId("shape-step").getByLabel("Header row").fill("3");
  await page.getByRole("button", { name: /^Next: Map Columns/ }).click();
  await expect(page.getByText(/Still missing/)).toHaveCount(0);
  await page.getByLabel("Map value to").selectOption("");
  await expect(page.getByText("Still missing: value (to go with metric)")).toBeVisible();
  await page.getByLabel("Map value to").selectOption("value");
  await page.getByRole("button", { name: "Next: Validate" }).click();

  // The server's check, before anything is imported, names the file's line 5.
  const issue = page.getByRole("cell", { name: mars.message });
  await expect(issue).toBeVisible();
  await expect(page.getByRole("row", { name: /MARS/ }).getByRole("cell").first()).toHaveText("5");
  await expect(page.getByText("All rows are valid")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Import Valid Rows Only (1)" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Back to Shape" })).toBeVisible();
  expect(sent[0]).toEqual(expect.objectContaining({ dry_run: true, row_lines: [4, 5],
    csv: "Department,Region,metric_id,value\nSALES,EMEA,opex_marketing,5\nSALES,MARS,opex_marketing,10" }));

  // Committed anyway, the refusal lists the same line and goes back, the
  // wizard's work kept.
  await page.getByRole("button", { name: "Import All Anyway" }).click();
  await page.getByRole("button", { name: /Commit Import \(\d+ rows?\)/ }).click();
  await expect(page.getByText("Import Failed")).toBeVisible();
  await expect(page.getByRole("row", { name: /MARS/ }).getByRole("cell").first()).toHaveText("5");
  expect(sent[1].row_lines).toEqual([4, 5]);
  await page.getByRole("button", { name: "Back", exact: true }).click();
  await expect(issue).toBeVisible();
});

test("records are imported into their table", async ({ page }) => {
  const sent: Array<{ url: string; csv: string }> = [];
  await page.route("**/api/forms/form-1/import", async (route) => {
    const body = route.request().postDataJSON();
    if (body.dry_run) return route.fulfill(json({ dry_run: true, rows: 1, error_rows: 0, errors: [] }));
    sent.push({ url: route.request().url(), csv: body.csv });
    await route.fulfill(json({ status: "ok", records_created: 1 }));
  });
  await page.route("**/api/import/upload", (route) => route.fulfill(json({ error: "the grid importer" }, 400)));
  await openWizard(page, "form", "Table", "Purchase Request", "Vendor,amount\nAcme,120\n");
  await commit(page);

  await expect(page.getByText("Import Complete")).toBeVisible();
  await expect(page.getByText("1 rows imported")).toBeVisible();
  expect(sent).toHaveLength(1);
  expect(sent[0].csv).toBe("vendor,amount\nAcme,120");
});
