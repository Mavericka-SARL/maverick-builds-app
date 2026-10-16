/**
 * A dashboard's selector bar: one selector per chosen dimension, drawn once
 * above the widgets. Every widget follows it and draws no selector of its own
 * for that dimension — one that opted out of the shared selection too, which
 * keeps its own selectors only for its other dimensions. The bar opens on the
 * developer's default member.
 *
 * Same self-contained route-mock style as dashboard-context-sync.spec.ts.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "bar-dash";
const GRID_ID = "bar-grid";

const dimensions = [
  { id: "dept", name: "Department", members: [{ id: "dept-a", code: "ENG", label: "Engineering" }] },
  {
    id: "period", name: "Period",
    members: [
      { id: "per-q1", code: "Q1", label: "Q1" },
      { id: "per-jan", code: "JAN", label: "January", parent_code: "Q1" },
      { id: "per-feb", code: "FEB", label: "February", parent_code: "Q1" },
    ],
  },
];

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, dimension_ids: ["dept", "period"] }],
  dimensions,
  departments: [],
  cells: { "m-cost:ENG:JAN": 100, "m-cost:ENG:FEB": 200 },
  totals: { "m-cost": 300 },
  access_rules: { dim_members: {}, metrics: {} },
};

// A and B follow the shared selection; C opted out, with both dimensions as
// selectors. The first widget to render used to draw the dashboard's one
// Period selector — with a bar, the bar draws it.
const dashboard = {
  id: DASH_ID,
  name: "Selector Bar Dashboard",
  tags: [],
  selector_bar: [{ dimension_id: "period", default: "FEB" }],
  widgets: [
    { id: "w-a", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 400, size_h: 300 },
    { id: "w-b", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 1, pos_x: 400, pos_y: 0, size_w: 400, size_h: 300 },
    { id: "w-c", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 2, pos_x: 800, pos_y: 0, size_w: 400, size_h: 300,
      widget_props: { sync_context: false, default_view: { rows: ["__metrics__"], cols: [], context: ["dept", "period"] } } },
    // A KPI following the bar: scoped by the bar's Period, the total over
    // Department (a "sync" tile would also take a Department member).
    { id: "w-k", widget_type: "metric_kpi", ref_id: "m-cost", content: null, sort_order: 3, pos_x: 0, pos_y: 320, size_w: 300, size_h: 120, widget_props: { kpi_context_mode: "bar" } },
  ],
};

async function mockBarDashboard(page: Page) {
  await page.route("**/api/dashboards", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([dashboard]) })
  );
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(dashboard) })
  );
  // A totals read answers by its scope: one value per Period member, and a
  // tell-tale 999 for any read scoped by Department.
  await page.route("**/api/grid*", (route) => {
    const scope = JSON.parse(new URL(route.request().url()).searchParams.get("scope") ?? "{}") as Record<string, string>;
    const total = scope.dept ? 999 : scope.period === "JAN" ? 100 : scope.period === "FEB" ? 200 : 300;
    return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ...gridData, totals: { "m-cost": total } }) });
  });
}

test("selector bar: one selector for the dashboard, followed by every widget", async ({ page }) => {
  await mockApi(page);
  await mockBarDashboard(page);
  await loadAs(page, "dept_head");

  await expect(page.getByText("Selector Bar Dashboard")).toBeVisible({ timeout: 15_000 });

  const bar = page.getByRole("toolbar", { name: "Dashboard selectors" });
  const barPeriod = bar.getByRole("button", { name: "Period", exact: true });
  // Opens on the developer's default (February), not the first leaf.
  await expect(barPeriod).toHaveText("February");
  // No widget draws a Period selector, the opted-out one included; that one
  // keeps its own Department selector.
  await expect(page.getByLabel("Period context")).toHaveCount(0);
  await expect(page.getByLabel("Department context")).toHaveCount(1);

  const cellOf = (widgetIdx: number) => page.locator("table").nth(widgetIdx).locator("tbody input").first();
  await expect(cellOf(0)).toHaveValue("200");
  await expect(cellOf(1)).toHaveValue("200");
  await expect(cellOf(2)).toHaveValue("200");
  const kpi = page.locator(".mvx-kpi__value");
  await expect(kpi).toHaveText("200");
  // The tile draws no selector: the bar is its selector.
  await expect(page.locator(".mvx-kpi__selectors")).toHaveCount(0);

  await barPeriod.click();
  await page.getByRole("listbox").getByRole("option", { name: "January" }).click();
  await expect(barPeriod).toHaveText("January");
  await expect(cellOf(0)).toHaveValue("100");
  await expect(cellOf(1)).toHaveValue("100");
  await expect(cellOf(2)).toHaveValue("100");
  await expect(kpi).toHaveText("100");
});
