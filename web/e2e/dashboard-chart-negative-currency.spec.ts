/**
 * A currency chart writes a negative amount with the sign before the symbol
 * (-$360K), as the KPI tile and the grid do. Its axis and bars read $-360K
 * (2026-10-23, a Budget vs actual page's variance chart), the same page
 * then writing one amount two ways.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test.use({ actionTimeout: 10_000 });

const DASH_ID = "variance-dash";
const REGION = "region";

const dashboard = {
  id: DASH_ID,
  name: "Variance Dashboard",
  tags: [],
  widgets: [{
    id: "w-variance", widget_type: "chart", ref_id: "grid-x", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 360,
    widget_props: { chart: { chart_type: "bar", dimension_id: REGION, metric_ids: ["m-rev"], value_format: "currency" } },
  }],
};

const chartPayload = {
  chart_type: "bar",
  as_of: "2026-10-23T00:00:00Z",
  categories: [{ key: "FR", label: "France" }, { key: "DE", label: "Germany" }],
  series: [{ metric_id: "m-rev", label: "Revenue", values: [457_000, -360_000], format: "currency", format_decimals: 0, format_currency: "$" }],
  context_dims: [],
  context: {},
};

test("a currency chart writes a negative amount as -$360K", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/dashboard-widgets/w-variance/chart-data", (route) => route.fulfill(json(chartPayload)));

  await loadAs(page, "dept_head");
  await expect(page.getByText("Variance Dashboard")).toBeVisible({ timeout: 15_000 });

  const chart = page.locator(".recharts-wrapper").first();
  await expect(chart).toBeVisible({ timeout: 15_000 });
  // The value axis runs below zero: its ticks are written -$…, never $-….
  const ticks = chart.locator(".recharts-cartesian-axis-tick-value");
  await expect(ticks.filter({ hasText: /^-\$/ }).first()).toBeVisible();
  await expect(chart.getByText(/\$-/)).toHaveCount(0);
});
