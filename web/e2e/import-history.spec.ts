/**
 * Recent Imports and the last step of a file import, as a self-hosting tester
 * reported them (2026-10-23):
 * - Recent Imports was missing while the model being built was not the
 *   application's default: the list was the default model's.
 * - A dimension's members and a table's records never showed there.
 * - It said nothing of what was imported, into what, from which file, by
 *   whom or how long it took.
 * - Incremental, chosen on the first step, was easy to forget by the last: a
 *   file imported again doubled its values.
 *
 * The server side is internal/gateway/import_history_test.go.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

async function openIntegrations(page: Page) {
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
}

test.beforeEach(async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
});

test("Recent Imports is the working model's, and says what each import went into, from which file, how, by whom and how long", async ({ page }) => {
  const asked: string[] = [];
  await page.route("**/api/import/jobs?*", (route) => {
    asked.push(new URL(route.request().url()).searchParams.get("revision_id") ?? "");
    return route.fulfill(json([
      { id: "job-3", model_id: "model-1", revision_name: "Working", status: 3, total_rows: 3, valid_rows: 3, error_rows: 0,
        created_at: "2026-10-23T10:02:00Z", source: "upload", target_type: "dimension", target_name: "Region",
        file_name: "regions.csv", by: "Anna", duration_ms: 1500 },
      { id: "job-2", model_id: "model-1", revision_name: "Working", status: 3, total_rows: 21000, valid_rows: 21000, error_rows: 0,
        created_at: "2026-10-23T10:01:00Z", source: "upload", target_type: "grid", target_name: "Sales", import_mode: "replace",
        file_name: "sales.xlsx", by: "Anna", duration_ms: 420 },
      { id: "job-1", model_id: "model-1", revision_name: "Working", status: 3, total_rows: 10, valid_rows: 10, error_rows: 0,
        created_at: "2026-10-23T10:00:00Z", source: "csv_import", target_type: "table", target_name: "Expenses",
        file_name: "expenses.csv", by: "Anna", duration_ms: 75000 },
    ]));
  });
  await openIntegrations(page);

  const history = page.getByRole("table").filter({ has: page.getByRole("columnheader", { name: "Imported into" }) });
  await expect(history.getByRole("row")).toHaveCount(4);
  const row = (i: number) => history.getByRole("row").nth(i);
  for (const text of ["Dimension Region", "regions.csv · Import Wizard", "Working", "Anna", "3 members", "1.5 s", "Committed"]) {
    await expect(row(1)).toContainText(text);
  }
  for (const text of ["Grid Sales", "sales.xlsx · Import Wizard", "Replace listed cells", "21000 values", "420 ms"]) {
    await expect(row(2)).toContainText(text);
  }
  for (const text of ["Table Expenses", "expenses.csv · saved import", "10 records", "1 min 15 s"]) {
    await expect(row(3)).toContainText(text);
  }
  // The working revision names the model.
  expect(asked).toContain("rev-1");
});

test("a model with no imports yet says so", async ({ page }) => {
  await page.route("**/api/import/jobs?*", (route) => route.fulfill(json([])));
  await openIntegrations(page);
  await expect(page.getByText("Recent Imports")).toBeVisible();
  await expect(page.getByText("No imports into this model yet.")).toBeVisible();
});

test("an incremental import shows its mode and asks before adding to the cells' values", async ({ page }) => {
  const sent: Array<{ import_mode?: string; grid_id?: string; file_name?: string }> = [];
  await page.route("**/api/import/upload", async (route) => {
    const body = route.request().postDataJSON();
    if (body.dry_run) return route.fulfill(json({ dry_run: true, rows: 1, values: 1, error_rows: 0, errors: [] }));
    sent.push(body);
    await route.fulfill(json({ job_id: "job-1", status: "committed", total_rows: 1, valid_rows: 1, error_rows: 0, revision_id: "rev-1" }));
  });
  await openIntegrations(page);
  await page.getByRole("button", { name: "New Import" }).click();
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "OPEX Planning Grid" });
  await page.getByLabel("Target revision").selectOption("rev-1");
  await expect(page.getByLabel(/Incremental/)).toBeChecked();
  await page.locator('input[type="file"]').setInputFiles({ name: "plan.csv", mimeType: "text/csv",
    buffer: Buffer.from("Department,Region,metric,value\nSALES,EMEA,opex_marketing,10\n") });
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: /^Next: Map Columns/ }).click();
  await page.getByRole("button", { name: "Next: Validate" }).click();
  await page.getByRole("button", { name: "Commit Import", exact: true }).click();

  await expect(page.getByText("Import mode", { exact: true }).locator("..")).toContainText("Incremental");
  await expect(page.getByText(/Incremental adds each value to the cell's current value/)).toBeVisible();

  // Cancel: nothing is sent.
  await page.getByRole("button", { name: /Commit Import \(\d+ rows?\)/ }).click();
  const ask = page.getByRole("alertdialog");
  await expect(ask).toContainText("Add to the cells' current values?");
  await ask.getByRole("button", { name: "Cancel" }).click();
  await expect(ask).toHaveCount(0);
  expect(sent).toHaveLength(0);

  // Confirmed: sent once, with what Recent Imports will say about it.
  await page.getByRole("button", { name: /Commit Import \(\d+ rows?\)/ }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Add to current values" }).click();
  await expect(page.getByText("Import Complete")).toBeVisible();
  expect(sent).toHaveLength(1);
  expect(sent[0]).toMatchObject({ import_mode: "incremental", file_name: "plan.csv" });
  expect(sent[0].grid_id).toBeTruthy();
});
