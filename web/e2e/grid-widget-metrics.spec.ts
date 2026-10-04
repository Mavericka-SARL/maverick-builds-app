/**
 * A grid widget shows the metrics its widget_props.metric_ids choose, in that
 * order, and lays its rows out in the saved default view's order. It used to
 * show every metric of its grid (a summary table of RF / LY / Var / Var %
 * carried the grid's helper metrics too), and rows saved as [region, dept]
 * rendered dept first (found rebuilding a CPG FP&A workbook, 2026-10-03).
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "chosen-metrics-dash";
const GRID_ID = "chosen-metrics-grid";

const metrics = [
  { id: "m-rf", name: "rf", label: "Rolling Forecast", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
  { id: "m-ly", name: "ly", label: "Prior Year", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
  { id: "m-share", name: "share", label: "Helper Share", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
];

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics,
  dimensions: [
    { id: "dept", name: "Department", members: [{ id: "d-eng", code: "ENG", label: "Engineering" }] },
    { id: "region", name: "Region", members: [{ id: "r-eu", code: "EU", label: "Europe" }] },
  ],
  departments: [],
  cells: { "m-rf:ENG:EU": 10, "m-ly:ENG:EU": 20, "m-share:ENG:EU": 30 },
  totals: { "m-rf": 10, "m-ly": 20, "m-share": 30 },
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = {
  id: DASH_ID,
  name: "Chosen Metrics Dashboard",
  tags: [],
  widgets: [
    {
      id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 700, size_h: 300,
      widget_props: {
        metric_ids: ["m-ly", "m-rf"],
        default_view: { rows: ["region", "dept"], cols: ["__metrics__"], context: [] },
      },
    },
  ],
};

async function mockDashboard(page: Page) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
}

test("a grid widget shows only its chosen metrics, in order, with rows in the saved order", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Chosen Metrics Dashboard")).toBeVisible({ timeout: 15_000 });

  const table = page.locator("table").first();
  const headers = table.locator("thead th");
  await expect(headers.filter({ hasText: "Prior Year" })).toHaveCount(1, { timeout: 15_000 });
  await expect(headers.filter({ hasText: "Rolling Forecast" })).toHaveCount(1);
  await expect(table.getByText("Helper Share")).toHaveCount(0);
  // Rows [region, dept]: region first, in the header and in each row.
  await expect(table.getByRole("columnheader", { name: "Region / Department" })).toBeVisible();
  await expect(table.getByRole("cell", { name: "Europe / Engineering" })).toBeVisible();
  // The chosen metrics follow in the chosen order.
  const texts = (await headers.allTextContents()).map((t) => t.trim());
  const at = (s: string) => texts.findIndex((t) => t.includes(s));
  expect(at("Prior Year")).toBeLessThan(at("Rolling Forecast"));
  // The first row's cells, in the chosen metric order.
  const inputs = table.locator("tbody input");
  await expect(inputs.nth(0)).toHaveValue("20");
  await expect(inputs.nth(1)).toHaveValue("10");
});
