/**
 * Pick-list cells (migration 110): a metric whose cells hold a member of a
 * dimension shows the member's label, offers the dimension's members in a
 * list and writes the chosen member's key; a calculated pick-list shows its
 * member; an input with agg_rule none shows no total; and a layout that
 * pins a dimension it puts in no zone still shows that dimension as a
 * selector (found rebuilding a sales target-setting workbook, 2026-10-04).
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "picklist-dash";
const GRID_ID = "picklist-grid";
const KEY = { draft: 1300000000000001, cancelled: 1300000000000002, within: 1300000000000003, review: 1300000000000004 };

const statusOptions = [
  { key: KEY.draft, code: "Draft", label: "Draft" },
  { key: KEY.cancelled, code: "Cancelled", label: "Cancelled" },
];
const reconOptions = [
  { key: KEY.within, code: "Within_Range", label: "Within Range" },
  { key: KEY.review, code: "Review", label: "Review" },
];
const metrics = [
  { id: "m-status", name: "act_status", label: "Status", is_input: true, agg_rule: "none", format: "picklist", format_decimals: 0,
    picklist_dimension_id: "statuses", picklist_options: statusOptions },
  { id: "m-index", name: "priority_index", label: "Priority Index", is_input: true, agg_rule: "none", format: "number", format_decimals: 2 },
  { id: "m-recon", name: "recon_status", label: "Reconciliation Status", is_input: false, agg_rule: "formula", format: "picklist",
    format_decimals: 0, picklist_dimension_id: "recon", picklist_options: reconOptions, aggregate_evaluated: true },
];

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics,
  dimensions: [
    { id: "activity", name: "activity", members: [
      { id: "a-all", code: "ALL", label: "All Activities" },
      { id: "a-1", code: "A1", label: "Retail expansion", parent_code: "ALL" },
      { id: "a-2", code: "A2", label: "Price change", parent_code: "ALL" },
    ] },
    { id: "year", name: "year", members: [{ id: "y-27", code: "Y27", label: "2027" }, { id: "y-28", code: "Y28", label: "2028" }] },
  ],
  departments: [],
  cells: {
    "m-status:A1:Y27": KEY.draft, "m-status:A2:Y27": KEY.cancelled,
    "m-index:A1:Y27": 1.1, "m-index:A2:Y27": 0.9,
    "m-recon:A1:Y27": KEY.within, "m-recon:A2:Y27": KEY.review, "m-recon:ALL:Y27": KEY.review,
  },
  totals: { "m-recon": KEY.review },
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = {
  id: DASH_ID,
  name: "Pick-list Dashboard",
  tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 900, size_h: 320,
    // year is pinned but put in no zone: it must still be a selector at Y27.
    widget_props: { default_view: { rows: ["activity"], cols: ["__metrics__"], context: [], filter_sel: { year: "Y27" } } },
  }],
};

async function mockDashboard(page: Page, writes: unknown[]) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
  await page.route("**/api/cells", async (route) => {
    writes.push(route.request().postDataJSON());
    await route.fulfill(json({ status: "ok" }));
  });
}

test("pick-list cells show, offer and write members; agg none has no total", async ({ page }) => {
  const writes: Array<Record<string, unknown>> = [];
  await mockApi(page);
  await mockDashboard(page, writes);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Pick-list Dashboard")).toBeVisible({ timeout: 15_000 });

  const table = page.locator("table").first();
  const a1 = table.getByRole("row", { name: /Retail expansion/ });
  const a2 = table.getByRole("row", { name: /Price change/ });
  const status = (row: typeof a1) => row.locator("select.mvx-cell-select");
  // The input pick-list shows its member and offers every member.
  await expect(status(a1)).toHaveValue(String(KEY.draft), { timeout: 15_000 });
  await expect(status(a2).locator("option:checked")).toHaveText("Cancelled");
  await expect(status(a1).locator("option")).toHaveText(["—", "Draft", "Cancelled"]);
  // The calculated pick-list shows its member, at the leaves and the total.
  await expect(a2).toContainText("Review");
  await expect(a1).toContainText("Within Range");
  const total = table.getByRole("row", { name: /All Activities/ });
  await expect(total).toContainText("Review");
  // agg_rule none: no total for the index (the sum 2.00 must not appear).
  await expect(total).not.toContainText("2.00");

  // Choosing a member writes its key.
  await status(a1).selectOption({ label: "Cancelled" });
  await expect.poll(() => writes.length).toBeGreaterThan(0);
  expect(writes[0]).toMatchObject({ metric_id: "m-status", value: KEY.cancelled, dim_codes: { activity: "A1", year: "Y27" } });

  // The pinned dimension in no zone is a selector at its pinned member.
  await expect(page.getByText("2027").first()).toBeVisible();
});
