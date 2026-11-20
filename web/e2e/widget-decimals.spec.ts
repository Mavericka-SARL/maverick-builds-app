/**
 * A widget's own Decimals setting (widget_props.decimals, owner 2026-11-20):
 * by default an amount of 1,000 or more is written whole (shownDecimals), and
 * a widget that asks for decimals writes every amount with exactly that many,
 * whatever its size, its metric's decimals and its number scale. Percentages
 * keep their own.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "decimals-dash";
const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

const gridData = {
  scenario: "Working", version: "draft",
  metrics: [
    { id: "m-amt", name: "amount", label: "Amount", is_input: false, agg_rule: "sum", format: "number", format_decimals: 1, dimension_ids: ["line"] },
    { id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "currency", format_currency: "€", format_decimals: 0, dimension_ids: [] },
    { id: "m-head", name: "headcount", label: "Headcount", is_input: true, agg_rule: "sum", format: "number", format_decimals: 1, dimension_ids: [] },
  ],
  dimensions: [{ id: "line", name: "line", label: "Line", format_property: "format", members: [
    { id: "REV", code: "REV", label: "Revenue" },
    { id: "GM", code: "GM", label: "Gross margin", properties: { format: "percentage" } },
    { id: "HC", code: "HC", label: "Headcount" },
  ] }],
  departments: [],
  cells: { "m-amt:REV": 21612, "m-amt:GM": 35, "m-amt:HC": 66 },
  totals: { "m-cost": 20418650.5, "m-head": 6 },
  access_rules: { dim_members: {}, metrics: {} },
};

const grid = (decimals?: number) => ({
  id: "w-grid", widget_type: "grid", ref_id: "dec-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 300,
  widget_props: { default_view: { rows: ["line"], cols: ["__metrics__"], context: [] }, metric_ids: ["m-amt"], hide_total_row: true, decimals },
});
const kpi = (id: string, refId: string, x: number, widget_props: Record<string, unknown>) => ({
  id, widget_type: "metric_kpi", ref_id: refId, content: null, sort_order: 1 + x / 300, pos_x: x, pos_y: 320, size_w: 300, size_h: 140,
  widget_props: { kpi_context_mode: "total", ...widget_props },
});

async function show(page: Page, widgets: unknown[]) {
  const dashboard = { id: DASH_ID, name: "Decimals Dashboard", tags: [], widgets };
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
  await loadAs(page, "dept_head");
}

const row = (page: Page, label: string) =>
  page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: new RegExp(`^${label}$`) }) });

test("a grid widget's Decimals writes every amount with that many; a percentage keeps its own", async ({ page }) => {
  await show(page, [grid(2)]);
  await expect(row(page, "Revenue").locator("td").last()).toHaveText("21,612.00", { timeout: 15_000 });
  await expect(row(page, "Headcount").locator("td").last()).toHaveText("66.00");
  await expect(row(page, "Gross margin").locator("td").last()).toHaveText("35.0%");
});

test("a grid widget without Decimals keeps the automatic rule", async ({ page }) => {
  await show(page, [grid()]);
  await expect(row(page, "Revenue").locator("td").last()).toHaveText("21,612", { timeout: 15_000 });
  await expect(row(page, "Headcount").locator("td").last()).toHaveText("66.0");
});

test("a KPI tile's Decimals wins over the whole-number rule and the scale's one decimal", async ({ page }) => {
  await show(page, [
    kpi("w-auto", "m-cost", 0, { number_scale: "none" }),
    kpi("w-two", "m-cost", 300, { number_scale: "none", decimals: 2 }),
    kpi("w-scaled", "m-cost", 600, { decimals: 2 }),
    kpi("w-none", "m-head", 900, { decimals: 0 }),
  ]);
  const values = page.locator(".mvx-kpi__value");
  await expect(values.filter({ hasText: /^€20,418,651$/ })).toHaveCount(1, { timeout: 15_000 });
  await expect(values.filter({ hasText: /^€20,418,650\.50$/ })).toHaveCount(1);
  await expect(values.filter({ hasText: /^€20\.42M$/ })).toHaveCount(1);
  await expect(values.filter({ hasText: /^6$/ })).toHaveCount(1);
});

test("a chart's Decimals writes its value labels with that many", async ({ page }) => {
  const chartWidget = (id: string, x: number, decimals?: number) => ({
    id, widget_type: "chart", ref_id: "dec-grid", content: null, sort_order: x / 600, pos_x: x, pos_y: 0, size_w: 600, size_h: 360,
    widget_props: { chart: { chart_type: "bar", dimension_id: "line", metric_ids: ["m-amt"], value_format: "number", show_values: true }, decimals },
  });
  const payload = {
    chart_type: "bar", as_of: "2026-11-20T00:00:00Z",
    categories: [{ key: "REV", label: "Revenue" }, { key: "HC", label: "Headcount" }],
    series: [{ metric_id: "m-amt", label: "Amount", values: [21_612.5, 4.25], format: "number", format_decimals: 1 }],
    context_dims: [], context: {},
  };
  const dashboard = { id: DASH_ID, name: "Decimals Dashboard", tags: [], widgets: [chartWidget("w-auto", 0), chartWidget("w-two", 600, 2)] };
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/dashboard-widgets/*/chart-data", (route) => route.fulfill(json(payload)));
  await loadAs(page, "dept_head");
  const charts = page.locator(".recharts-wrapper");
  await expect(charts).toHaveCount(2, { timeout: 15_000 });
  // Automatic: 1,000 or more whole, a small value its own decimals.
  await expect(charts.nth(0).getByText("21,613", { exact: true })).toBeVisible();
  await expect(charts.nth(0).getByText("4.25", { exact: true })).toBeVisible();
  // Two decimals on every value.
  await expect(charts.nth(1).getByText("21,612.50", { exact: true })).toBeVisible();
  await expect(charts.nth(1).getByText("4.25", { exact: true })).toBeVisible();
});
