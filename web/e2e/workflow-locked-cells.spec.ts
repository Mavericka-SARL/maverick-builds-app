/**
 * Cells a workflow has locked are drawn read-only, with the reason a write
 * would be refused with (found preparing a how-to video, 2026-10-25: the
 * grid drew them editable, and a business user learned that Europe's budget
 * was approved only when a save failed). The lock is the crossing of the
 * instance's fields: Europe · Budget locks France's and Germany's budget,
 * not their actuals and not the United States.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "locked-cells-dash";
const GRID_ID = "locked-cells-grid";
const REASON = "Europe · Budget is locked: a workflow about it is awaiting approval or was approved";

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-units", name: "units", label: "Units", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 }],
  dimensions: [
    {
      id: "region", name: "Region",
      members: [
        { id: "r-eu", code: "EUROPE", label: "Europe" },
        { id: "r-fr", code: "FR", label: "France", parent_code: "EUROPE" },
        { id: "r-de", code: "DE", label: "Germany", parent_code: "EUROPE" },
        { id: "r-am", code: "AMER", label: "Americas" },
        { id: "r-us", code: "US", label: "United States", parent_code: "AMER" },
      ],
    },
    {
      id: "version", name: "Version",
      members: [
        { id: "v-b", code: "BUDGET", label: "Budget" },
        { id: "v-a", code: "ACTUAL", label: "Actual" },
      ],
    },
  ],
  departments: [],
  cells: { "m-units:FR:BUDGET": 10, "m-units:FR:ACTUAL": 7, "m-units:US:BUDGET": 5 },
  totals: { "m-units": 22 },
  access_rules: { dim_members: {}, metrics: {} },
  workflow_locks: [{
    fields: [{ region: ["DE", "EUROPE", "FR"] }, { version: ["BUDGET"] }],
    metric_ids: ["m-units"],
    reason: REASON,
  }],
};

const dashboard = {
  id: DASH_ID,
  name: "Locked Cells Dashboard",
  tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 800, size_h: 320,
    widget_props: { default_view: { rows: ["region"], cols: ["version"], context: ["__metrics__"] } },
  }],
};

async function mockDashboard(page: Page) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
}

test("cells a workflow locked are read-only with the reason; the rest stay editable", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Locked Cells Dashboard")).toBeVisible({ timeout: 15_000 });

  const row = (label: string) => page.locator("table").first().locator("tbody tr", { has: page.getByText(label, { exact: true }) });
  await expect(row("France")).toHaveCount(1, { timeout: 15_000 });
  // Columns: the label, then Budget, Actual.
  const franceBudget = row("France").locator("td").nth(1);
  await expect(franceBudget.locator("input")).toHaveCount(0);
  await expect(franceBudget).toHaveAttribute("title", new RegExp(`^${REASON}`));
  await expect(row("Germany").locator("td").nth(1).locator("input")).toHaveCount(0);
  // France's actuals and the United States' budget are outside the lock.
  await expect(row("France").locator("td").nth(2).locator("input")).toBeVisible();
  await expect(row("United States").locator("td").nth(1).locator("input")).toBeVisible();
});
