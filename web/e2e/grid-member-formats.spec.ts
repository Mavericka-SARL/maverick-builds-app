/**
 * A line dimension whose members carry their own number format (its
 * format_property, migration 190): one metric over lines that mix amounts and
 * a margin shows the margin row as a percentage, the others as numbers
 * (Integrated FP&A, 2026-11-19: "€35.0" for a 35.0% gross margin).
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "fmt-dash";
const gridData = {
  scenario: "Working", version: "draft",
  metrics: [{ id: "m-bva", name: "bva", label: "Budget vs actual", is_input: false, agg_rule: "none", format: "number",
    format_decimals: 1, dimension_ids: ["line"] }],
  dimensions: [{ id: "line", name: "bv_line", label: "Line", format_property: "format", members: [
    { id: "REV", code: "REV", label: "Revenue", properties: { format: "number" } },
    { id: "GM", code: "GM", label: "Gross margin", properties: { format: "percentage" } },
    { id: "HC", code: "HC", label: "Headcount" },
  ] }],
  departments: [],
  cells: { "m-bva:REV": 21612, "m-bva:GM": 35, "m-bva:HC": 66 },
  totals: {},
  access_rules: { dim_members: {}, metrics: {} },
};
const dashboard = {
  id: DASH_ID, name: "Format Dashboard", tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: "fmt-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 300,
    widget_props: { default_view: { rows: ["line"], cols: ["__metrics__"], context: [] }, hide_total_row: true },
  }],
};

test("a calculated member's own format wins over its row's", async ({ page }) => {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const twoDims = {
    ...gridData,
    metrics: [{ ...gridData.metrics[0], dimension_ids: ["line", "view"] }],
    dimensions: [gridData.dimensions[0], { id: "view", name: "bva_view", label: "Comparison", format_property: "format", members: [
      { id: "BUD", code: "BUD", label: "Budget" }, { id: "ACT", code: "ACT", label: "Actual" },
      { id: "VAR", code: "VAR", label: "Variance", formula: "{ACT} - {BUD}" },
      { id: "VARPCT", code: "VARPCT", label: "Variance %", formula: "IF({BUD} = 0, 0, ({ACT} - {BUD}) / ABS({BUD}) * 100)", properties: { format: "percentage" } },
    ] }],
    cells: { "m-bva:REV:BUD": 200, "m-bva:REV:ACT": 210, "m-bva:GM:BUD": 30, "m-bva:GM:ACT": 35, "m-bva:HC:BUD": 60, "m-bva:HC:ACT": 66 },
  };
  const dash = { ...dashboard, widgets: [{ ...dashboard.widgets[0],
    widget_props: { default_view: { rows: ["line"], cols: ["view"], context: [] }, hide_total_row: true } }] };
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dash])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dash)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(twoDims)));
  await loadAs(page, "dept_head");
  const row = (label: string) => page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: new RegExp(`^${label}$`) }) });
  await expect(row("Revenue").locator("td").last()).toHaveText("5.0%", { timeout: 15_000 });
  await expect(row("Gross margin").locator("td").nth(3)).toHaveText("5.0 pp");
});

test("a member's own format formats its row; the others keep the metric's", async ({ page }) => {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
  await loadAs(page, "dept_head");
  const row = (label: string) => page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: new RegExp(`^${label}$`) }) });
  await expect(row("Gross margin").locator("td").last()).toHaveText("35.0%", { timeout: 15_000 });
  await expect(row("Revenue").locator("td").last()).toHaveText("21,612.0");
  await expect(row("Headcount").locator("td").last()).toHaveText("66.0");
});
