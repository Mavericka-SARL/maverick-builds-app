/**
 * Run Import on a saved file integration opens the Import Wizard on the
 * saved column map; the person may edit the mapping, and the wizard sends
 * the file as its own Map step wrote it — columns under the model's names.
 * It says so ("mapped": true), for Validate's dry run and for the commit,
 * so the server does not apply the saved map a second time: applied twice,
 * it read the model's names as source columns and dropped a renamed one
 * (2026-10-19). A dashboard's button sends the person's file, unmapped
 * (ai-file-integration.spec.ts).
 *
 * The server side is TestWizardRunOfASavedIntegrationIsMappedOnce
 * (internal/gateway/integration_file_run_test.go).
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

test("Run Import sends the file the wizard mapped, marked mapped, to Validate and to the commit", async ({ page }) => {
  await mockApi(page);
  const checks: Array<Record<string, unknown>> = [];
  const runs: Array<Record<string, unknown>> = [];
  await page.route(url => url.pathname === "/api/integrations/int-1/run", async (route) => {
    const body = route.request().postDataJSON();
    if (route.request().url().includes("dry_run=true")) {
      checks.push(body);
      return route.fulfill(json({ dry_run: true, rows: 1, values: 1, error_rows: 0, errors: [] }));
    }
    runs.push(body);
    await route.fulfill(json({ rows_imported: 1, values_imported: 1, error_rows: 0, errors: [] }));
  });
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "Run Import" }).first().click();
  await page.getByLabel("Target revision").selectOption("rev-1");
  await page.locator('input[type="file"]').setInputFiles({ name: "plan.csv", mimeType: "text/csv",
    buffer: Buffer.from("Department,Region,metric,value\nSALES,EMEA,opex_marketing,10\n") });
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: /^Next: Map Columns/ }).click();
  // The person edits the saved map: Department was not in it.
  await page.getByLabel("Map Department to").selectOption({ label: "dim: Department" });
  await page.getByRole("button", { name: "Next: Validate" }).click();

  await expect.poll(() => checks.length).toBe(1);
  expect(checks[0].mapped).toBe(true);

  await page.getByRole("button", { name: /Commit Import/ }).first().click();
  await page.getByRole("button", { name: /Commit Import \(\d+ rows?\)/ }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Add to current values" }).click();
  await expect.poll(() => runs.length).toBe(1);
  expect(runs[0].mapped).toBe(true);
  expect(runs[0].file_name).toBe("plan.csv");
});
