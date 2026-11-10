/**
 * Total rules (owner's decision 2026-11-09): a dimension of scenarios
 * totals as its first member (Base), not as Base + Downside, and a metric
 * may set its own rule over it — added up, or no total at all. The grid's
 * Total row follows them as the server's totals do.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "rules-total-dash";
const member = (code: string, label: string) => ({ id: code, code, label });
const metric = (id: string, name: string, label: string) =>
  ({ id, name, label, is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 });
const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [metric("m-units", "units", "Units"), metric("m-heads", "heads", "Heads"), metric("m-index", "index", "Index")],
  dimensions: [{
    id: "scenario", name: "scenario", label: "Scenario", display_level: null,
    members: [member("BASE", "Base"), member("DOWN", "Downside")],
    total_rule: "first", metric_rules: { "m-heads": "sum", "m-index": "none" },
  }],
  departments: [],
  cells: { "m-units:BASE": 10, "m-units:DOWN": 1, "m-heads:BASE": 5, "m-heads:DOWN": 6, "m-index:BASE": 2, "m-index:DOWN": 3 },
  totals: { "m-units": 10, "m-heads": 11 },
  access_rules: { dim_members: {}, metrics: {} },
};
const dashboard = {
  id: DASH_ID, name: "Rules Total Dashboard", tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: "rules-total-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 700, size_h: 300,
    widget_props: { default_view: { rows: ["scenario"], cols: ["__metrics__"], context: [] } },
  }],
};

test("a scenario dimension's Total is Base's; a metric's own rule adds up or gives none", async ({ page }) => {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
  await loadAs(page, "dept_head");

  const grid = page.locator(".mvx-dash-cell").first();
  await expect(grid.locator("tbody td", { hasText: /^Downside$/ })).toHaveCount(1, { timeout: 15_000 });
  const total = grid.locator("tbody tr", { has: page.locator("td", { hasText: /^Total$/ }) });
  await expect(total).toHaveCount(1);
  const cells = total.locator("td");
  // Label, then Units, Heads, Index.
  await expect(cells.nth(1)).toHaveText("10");
  await expect(cells.nth(2)).toHaveText("11");
  await expect(cells.nth(3)).not.toHaveText(/\d/);
});
