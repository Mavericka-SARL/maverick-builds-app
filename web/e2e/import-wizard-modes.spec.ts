/**
 * A file imported into a grid again — corrected, or the same one sent twice
 * — can replace the cells it lists. File imports offered only Incremental,
 * described as "Add / update rows" although it adds each value to the cell's
 * (so a second import doubled it), and Full reload, which wipes the
 * revision.
 *
 * The server side is internal/importpkg/commit_rows_test.go.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

test("a file import can replace the cells it lists, and says that incremental adds", async ({ page }) => {
  await mockApi(page);
  const sent: Array<{ import_mode?: string }> = [];
  await page.route("**/api/import/upload", async (route) => {
    sent.push(route.request().postDataJSON());
    await route.fulfill(json({ job_id: "job-1", status: "committed", total_rows: 1, valid_rows: 1, error_rows: 0, revision_id: "rev-1" }));
  });
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "New Import" }).click();
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "OPEX Planning Grid" });
  await page.getByLabel("Target revision").selectOption("rev-1");

  await expect(page.getByText("importing the same file again adds it again")).toBeVisible();
  await page.getByLabel(/Replace listed cells/).check();
  await page.locator('input[type="file"]').setInputFiles({ name: "plan.csv", mimeType: "text/csv",
    buffer: Buffer.from("Department,Region,metric,value\nSALES,EMEA,opex_marketing,10\n") });
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: /^Next: Map Columns/ }).click();
  await page.getByRole("button", { name: "Next: Validate" }).click();
  await page.getByRole("button", { name: /Commit Import/ }).first().click();
  await page.getByRole("button", { name: /Commit Import \(\d+ rows?\)/ }).click();

  await expect.poll(() => sent.length).toBe(1);
  expect(sent[0].import_mode).toBe("replace");
});
