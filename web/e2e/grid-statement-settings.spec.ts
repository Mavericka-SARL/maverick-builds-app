/**
 * A read-only statement's settings (production review, 2026-11-07):
 *  - hide_legend, hide_badges, label_width and footer on a grid widget;
 *  - a metric's row_style "total" draws its row as a total;
 *  - a percentage at a difference member ({ACT} - {BUD}) shows in points;
 *  - hide_total_row leaves out the Total row of that grid only.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "statement-dash";
const member = (code: string, label: string, extra: Record<string, unknown> = {}) => ({ id: code, code, label, ...extra });
const views = [member("ACT", "Actual"), member("BUD", "Budget"), member("VAR", "Actual vs budget", { formula: "{ACT} - {BUD}" })];
const statement = {
  scenario: "Working",
  version: "draft",
  metrics: [
    { id: "m-rev", name: "revenue", label: "Revenue", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
    { id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
    { id: "m-ebitda", name: "ebitda", label: "EBITDA", is_input: false, formula: "revenue - cost", agg_rule: "sum", format: "number", format_decimals: 0, row_style: "total" },
    { id: "m-margin", name: "margin_pct", label: "Margin %", is_input: true, agg_rule: "average", format: "percentage", format_decimals: 1 },
  ],
  dimensions: [{ id: "view", name: "View", members: views }],
  departments: [],
  cells: { "m-rev:ACT": 1000, "m-rev:BUD": 900, "m-cost:ACT": 600, "m-cost:BUD": 500, "m-margin:ACT": 40, "m-margin:BUD": 44.4 },
  totals: {},
  access_rules: { dim_members: {}, metrics: {} },
};
const products = Array.from({ length: 3 }, (_, i) => member(`P${i + 1}`, `Product ${i + 1}`));
const prices = {
  ...statement,
  metrics: [{ id: "m-price", name: "price", label: "Price", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 }],
  dimensions: [{ id: "product", name: "Product", members: products }],
  cells: { "m-price:P1": 10, "m-price:P2": 20, "m-price:P3": 30 },
};

const widget = (id: string, ref: string, pos_y: number, props: Record<string, unknown>) => ({
  id, widget_type: "grid", ref_id: ref, content: null, sort_order: pos_y, pos_x: 0, pos_y, size_w: 900, size_h: 260, widget_props: props,
});
const dashboard = {
  id: DASH_ID,
  name: "Statement Dashboard",
  tags: [],
  widgets: [
    widget("w-stmt", "stmt-grid", 0, { hide_legend: true, hide_badges: true, label_width: 300, footer: "Source: the general ledger",
      default_view: { rows: ["__metrics__"], cols: ["view"], context: [] } }),
    widget("w-prices", "price-grid", 280, { hide_total_row: true, default_view: { rows: ["product"], cols: ["__metrics__"], context: [] } }),
    widget("w-prices2", "price-grid", 560, { default_view: { rows: ["product"], cols: ["__metrics__"], context: [] } }),
  ],
};

async function mockDashboard(page: Page) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) =>
    route.fulfill(json(new URL(route.request().url()).searchParams.get("grid_def_id") === "stmt-grid" ? statement : prices)));
}

test("a statement grid without legend or tags, with a footer, a wide label column, a total row and points", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  const stmt = page.locator(".mvx-dash-cell").nth(0);
  await expect(stmt.locator("tbody td", { hasText: "EBITDA" })).toHaveCount(1, { timeout: 15_000 });
  await expect(stmt.getByText("Source: the general ledger")).toBeVisible();
  await expect(stmt.getByLabel("Cell legend")).toHaveCount(0);
  await expect(stmt.getByText("CALC", { exact: true })).toHaveCount(0);
  const labelWidth = await stmt.locator("table.mvx-grid-frozen thead th").first().evaluate(th => Math.round(th.getBoundingClientRect().width));
  expect(labelWidth).toBeGreaterThanOrEqual(295);
  await expect(stmt.locator("tr.mvx-grid-row--total")).toHaveCount(1);
  await expect(stmt.locator("tr.mvx-grid-row--total td").first()).toHaveText("EBITDA");
  // Margin % at Actual vs budget: 40 - 44.4 = -4.4 points.
  const marginRow = stmt.locator("tbody tr", { hasText: "Margin %" });
  await expect(marginRow).toContainText("-4.4 pp");
  // The typed cells keep the metric's own format.
  await expect(marginRow.locator("input").first()).toHaveValue("40.0%");
});

test("hide_total_row leaves out that grid's Total row only", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  const without = page.locator(".mvx-dash-cell").nth(1);
  const withTotal = page.locator(".mvx-dash-cell").nth(2);
  await expect(withTotal.locator("tbody td", { hasText: "Product 3" })).toHaveCount(1, { timeout: 15_000 });
  await expect(withTotal.locator("tbody td", { hasText: /^Total$/ })).toHaveCount(1);
  await expect(without.locator("tbody td", { hasText: "Product 3" })).toHaveCount(1);
  await expect(without.locator("tbody td", { hasText: /^Total$/ })).toHaveCount(0);
});
