/**
 * Right-clicking an input cell in the planning grid opens its change
 * history: every value newest first, who entered it, how, and — for values
 * later removed — why. Enterprise: other editions get the gate instead.
 * Uses the calc-cells smoke fixture's flat grid so a cell is unambiguous.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs, enterpriseLicense } from "./mocks";

const DASH_ID = "hist-dash";
const GRID_ID = "hist-grid";
const dimensions = [{ id: "dept", name: "Department", members: [
  { id: "dept-mkt", code: "MKT", label: "Marketing" }, { id: "dept-sales", code: "SALES", label: "Sales" },
] }];
const metrics = [
  { id: "m-mkt", name: "opex_marketing", label: "Marketing Spend", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
  { id: "m-tot", name: "total_opex", label: "Total OPEX", is_input: false, formula: "=opex_marketing*2", agg_rule: "sum", format: "number", format_decimals: 0 },
];
const gridData = {
  scenario: "Working", version: "draft", metrics, dimensions, departments: [],
  cells: { "m-mkt:MKT": 300, "m-mkt:SALES": 90000, "m-tot:MKT": 600, "m-tot:SALES": 180000 },
  totals: { "m-mkt": 90300, "m-tot": 180600 }, access_rules: { dim_members: {}, metrics: {} },
};
const dashboard = { id: DASH_ID, name: "History Dashboard", tags: [], widgets: [{ id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, col_start: 1, col_span: 12 }] };

async function mockGrid(page: Page) {
  await page.route("**/api/dashboards", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([dashboard]) }));
  await page.route(`**/api/dashboards/${DASH_ID}`, (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(dashboard) }));
  await page.route("**/api/grid*", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(gridData) }));
}

async function openHistory(page: Page) {
  await expect(page.getByText("History Dashboard")).toBeVisible({ timeout: 15_000 });
  const row = page.locator("table").first().getByRole("row", { name: /Marketing Spend/ });
  await row.getByRole("cell").nth(1).click({ button: "right" });
}

test("enterprise: the history drawer lists every value, marks the current one and explains a removal", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await mockGrid(page);
  await loadAs(page, "dept_head");
  await openHistory(page);
  await expect(page.getByText(/History — Marketing Spend · Marketing/)).toBeVisible();
  const table = page.getByTestId("cell-history");
  await expect(table.getByTestId("cell-history-current")).toContainText("300");
  await expect(table.getByTestId("cell-history-current")).toContainText("current");
  await expect(table.getByTestId("cell-history-current")).toContainText("Alex Smith");
  const deleted = table.getByTestId("cell-history-deleted");
  await expect(deleted).toContainText("250");
  await expect(deleted).toContainText("Bob Lee");
  await expect(deleted).toContainText("removed by an import in full-reload mode");
  await expect(table.getByTestId("cell-history-row")).toContainText("form: Post expenses");
  await page.keyboard.press("Escape");
  await expect(table).toHaveCount(0);
});

test("community: right-click shows which edition unlocks history, not the data", async ({ page }) => {
  await mockApi(page);
  await mockGrid(page);
  await loadAs(page, "dept_head");
  await openHistory(page);
  const gate = page.locator(".mvx-feature-gate");
  await expect(gate).toContainText("Cell history");
  await expect(gate).toContainText("requires the Enterprise edition");
  await expect(page.getByTestId("cell-history")).toHaveCount(0);
});

test("a calculated cell has no history affordance", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await mockGrid(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("History Dashboard")).toBeVisible({ timeout: 15_000 });
  const calc = page.locator("table").first().getByRole("row", { name: /Total OPEX/ }).getByRole("cell").nth(1);
  await expect(calc).not.toHaveAttribute("title", /history/i);
});
