/**
 * Calculated members in the planning grid: a Scenario dimension's VAR =
 * {RF} - {LY} and VARPCT = IF({LY} = 0, 0, ({RF} - {LY}) / ABS({LY}) * 100)
 * are computed from their siblings at every coordinate the grid shows — at
 * the months and at the FY total, where the variance % is (210 - 160) / 160
 * = 31.25, never the months' 20 % + 50 % added up. The server sends only RF
 * and LY here; the grid works out the rest, and a calculated cell takes no
 * input.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "calc-members-dash";
const GRID_ID = "calc-members-grid";

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-sales", name: "sales", label: "Sales", is_input: true, agg_rule: "sum", format: "number", format_decimals: 2 }],
  dimensions: [
    {
      id: "scenario", name: "Scenario",
      members: [
        { id: "s-rf", code: "RF", label: "Forecast" },
        { id: "s-ly", code: "LY", label: "Prior year" },
        { id: "s-var", code: "VAR", label: "Variance", formula: "{RF} - {LY}" },
        { id: "s-pct", code: "VARPCT", label: "Variance %", formula: "IF({LY} = 0, 0, ({RF} - {LY}) / ABS({LY}) * 100)" },
      ],
    },
    {
      id: "month", name: "Month",
      members: [
        { id: "mo-fy", code: "FY", label: "FY" },
        { id: "mo-1", code: "M1", label: "Jan", parent_code: "FY" },
        { id: "mo-2", code: "M2", label: "Feb", parent_code: "FY" },
      ],
    },
  ],
  departments: [],
  cells: { "m-sales:RF:M1": 120, "m-sales:RF:M2": 90, "m-sales:LY:M1": 100, "m-sales:LY:M2": 60 },
  totals: { "m-sales": 370 },
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = {
  id: DASH_ID,
  name: "Calculated Members Dashboard",
  tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 800, size_h: 320,
    widget_props: { default_view: { rows: ["scenario"], cols: ["month"], context: ["__metrics__"] } },
  }],
};

async function mockDashboard(page: Page) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
}

test("calculated members are computed from their siblings at the months and at FY", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Calculated Members Dashboard")).toBeVisible({ timeout: 15_000 });

  const row = (label: string) => page.locator("table").first().locator("tbody tr", { has: page.getByText(label, { exact: true }) });
  const cellsOf = async (label: string) => (await row(label).locator("td").allTextContents()).map((t) => t.trim());
  await expect(row("Variance")).toHaveCount(1, { timeout: 15_000 });
  // Columns: the label, then FY, Jan, Feb (FY's children follow it).
  const variance = await cellsOf("Variance");
  const pct = await cellsOf("Variance %");
  expect(variance.join(" | ")).toContain("50.00");   // FY: 210 - 160
  expect(variance.join(" | ")).toContain("20.00");   // Jan
  expect(variance.join(" | ")).toContain("30.00");   // Feb
  expect(pct.join(" | ")).toContain("31.25");        // FY, from the FY totals
  expect(pct.join(" | ")).not.toContain("70.00");    // never 20 + 50
  expect(pct.join(" | ")).toContain("20.00");        // Jan
  expect(pct.join(" | ")).toContain("50.00");        // Feb
  // A calculated cell takes no input.
  await expect(row("Variance").locator("input")).toHaveCount(0);
  await expect(row("Forecast").locator("input").first()).toBeVisible();
});
