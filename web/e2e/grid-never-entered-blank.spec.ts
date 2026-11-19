/**
 * An input cell nobody entered is blank in the grid, not 0 (Integrated FP&A
 * build, 2026-11-19: Product A's external service % — never entered — read
 * "0.0%" beside Service's 20.0%, editable and read-only alike, while the
 * engine and the workbook treat blank and 0 differently).
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "blank-dash";
const member = (code: string, label: string) => ({ id: code, code, label });
const gridData = (readonly: boolean) => ({
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-ext", name: "d_ext_pct", label: "External %", is_input: true, agg_rule: "none", format: "percentage",
    format_decimals: 1, dimension_ids: ["product"], readonly }],
  dimensions: [{ id: "product", name: "product", label: "Revenue line",
    members: [member("PROD_A", "Product A"), member("SERVICE", "Service"), member("PROD_B", "Product B")] }],
  departments: [],
  cells: { "m-ext:SERVICE": 20, "m-ext:PROD_B": 0 },
  totals: {},
  access_rules: { dim_members: {}, metrics: {} },
});
const dashboard = {
  id: DASH_ID, name: "Blank Dashboard", tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: "blank-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 300,
    widget_props: { default_view: { rows: ["product"], cols: ["__metrics__"], context: [] }, hide_total_row: true },
  }],
};

async function setup(page: Page, readonly: boolean) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData(readonly))));
  await loadAs(page, "dept_head");
}

const row = (page: Page, label: string) =>
  page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: new RegExp(`^${label}$`) }) });

test("an editable input nobody entered is empty; an entered 0 shows 0", async ({ page }) => {
  await setup(page, false);
  await expect(row(page, "Service").locator("input")).toHaveValue("20.0%", { timeout: 15_000 });
  await expect(row(page, "Product A").locator("input")).toHaveValue("");
  await expect(row(page, "Product B").locator("input")).toHaveValue("0.0%");
});

test("a read-only input nobody entered shows a dash, not 0", async ({ page }) => {
  await setup(page, true);
  await expect(row(page, "Service").locator("td").last()).toHaveText("20.0%", { timeout: 15_000 });
  await expect(row(page, "Product A").locator("td").last()).toHaveText("—");
  await expect(row(page, "Product B").locator("td").last()).toHaveText("0.0%");
});

test("with metrics on rows, the row header names the dimension by its label", async ({ page }) => {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const onRows = { ...dashboard, widgets: [{ ...dashboard.widgets[0],
    widget_props: { default_view: { rows: ["product", "__metrics__"], cols: [], context: [] } } }] };
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([onRows])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(onRows)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData(false))));
  await loadAs(page, "dept_head");
  const head = page.locator("table").first().locator("thead");
  await expect(head).toContainText("Revenue line / Metric", { timeout: 15_000 });
  await expect(head).not.toContainText("product / Metric");
});
