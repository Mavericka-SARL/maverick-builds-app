/**
 * Integrations › Workbook reconciliation: the workbook kept with the revision, its map
 * (proposed from the workbook, then saved) and cases, and the last run's
 * report — matches per case, the map's own problems apart, the differences
 * with their cell, members, workbook and model values (2026-11-21).
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
const WORKING = { id: "rev-w", name: "Working", created_at: "2026-09-01T00:00:00Z", is_active: false };

const RULE = { name: "Plan", sheet: "Plan", rows: { from: 2, to: 3 }, metrics_by_column: { B: "price", D: "revenue" },
  members: { item: { column: "A" }, scen: { case: true } } };

test("reconcile: propose a map, save it, run, read the report", async ({ page }) => {
  let saved: unknown;
  let runs = 0;
  let status = "done";
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([WORKING])));
  await page.route("**/api/developer/reconciliation?*", (route) => route.fulfill(json({
    recalculation: false,
    reconciliation: {
      revision_id: "rev-w", file_name: "plan.xlsx", workbook_size: 20480, sheets: ["Plan", "Inputs"], uploaded_at: "2026-11-21T10:00:00Z",
      map: (saved as { map?: unknown } | undefined)?.map ?? { rules: [] }, cases: [{ name: "Base", members: { scen: "BASE" } }], status,
      report: status === "running" ? undefined : {
        ran_at: "2026-11-21T10:05:00Z", compared: 6, matched: 5, differences_total: 1,
        mapping_problems: ['Plan: A4 "C" names no member of item'],
        cases: [
          { name: "Base", status: "compared", compared: 6, matched: 5, by_sheet: [], by_metric: [{ name: "revenue", compared: 2, matched: 1 }, { name: "price", compared: 2, matched: 2 }] },
          { name: "High", status: "skipped", reason: "this case changes the workbook's inputs, and recalculating a workbook needs LibreOffice", compared: 0, matched: 0, by_sheet: [], by_metric: [] },
        ],
        differences: [{ case: "Base", sheet: "Plan", cell: "D3", metric: "revenue", members: { item: "B", scen: "BASE" }, expected: "61", actual: "60" }],
      },
    },
  })));
  await page.route("**/api/developer/reconciliation/propose", (route) => route.fulfill(json({
    map: { rules: [RULE] }, notes: ['Plan: scen is not bound. Its members are not added up (total "first"): bind it to the case.'],
  })));
  await page.route("**/api/developer/reconciliation/map", async (route) => {
    saved = route.request().postDataJSON();
    await route.fulfill(json({ saved: true }));
  });
  await page.route("**/api/developer/reconciliation/run", async (route) => {
    runs++;
    status = "running";
    await route.fulfill(json({ status: "running" }, 202));
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Integrations", exact: true }).click();
  await page.getByRole("button", { name: /Workbook reconciliation/ }).click();
  await expect(page.getByText("plan.xlsx")).toBeVisible({ timeout: 15_000 });

  // The report as it stands.
  await expect(page.getByText("5 / 6 cells match")).toBeVisible();
  await expect(page.getByText("revenue (1)")).toBeVisible();
  await expect(page.getByText(/not compared: this case changes the workbook's inputs/)).toBeVisible();
  await expect(page.getByText('Plan: A4 "C" names no member of item')).toBeVisible();
  const diff = page.getByRole("row", { name: /Plan!D3/ });
  await expect(diff).toContainText("item=B, scen=BASE");
  await expect(diff).toContainText("61");
  await expect(page.getByText(/does not recalculate workbooks/)).toBeVisible();

  // Propose, then save what was proposed.
  await page.getByRole("button", { name: "Propose from the workbook" }).click();
  await expect(page.getByText(/scen is not bound/)).toBeVisible();
  await expect(page.getByRole("row", { name: /column A/ })).toContainText("scen ← the case");
  await page.getByRole("button", { name: "Save map and cases" }).click();
  await expect.poll(() => (saved as { map?: { rules?: unknown[] } })?.map?.rules?.length).toBe(1);

  // Run: the page follows it.
  await page.getByRole("button", { name: "Run", exact: true }).click();
  await expect.poll(() => runs).toBe(1);
  await expect(page.getByRole("button", { name: /Comparing/ })).toBeVisible();
});
