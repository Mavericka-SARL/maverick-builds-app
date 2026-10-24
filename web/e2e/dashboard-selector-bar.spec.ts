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

// A member list read before the default member was added (a page opened
// earlier, a member added in Developer › Dimensions since) opened the bar on
// the first leaf, and it stayed there once the fresh list arrived
// (2026-10-23: Budget vs actual on France, not All regions). The member the
// bar fell back to gives way to the default; one the viewer chose does not.
// The list is read again as when the person comes back to the tab past its
// 30-second freshness.
async function rereadMembers(page: Page) {
  await page.clock.fastForward(31_000);
  await page.evaluate(() => window.dispatchEvent(new Event("visibilitychange")));
}

for (const viewerChose of [false, true]) {
  test(`selector bar: a default added after the members were read ${viewerChose ? "leaves the viewer's choice" : "is applied once they are read again"}`, async ({ page }) => {
    await page.clock.install();
    await mockApi(page);
    await mockBarDashboard(page);
    const withMarch = { ...dimensions[1], members: [...dimensions[1].members, { id: "per-mar", code: "MAR", label: "March", parent_code: "Q1" }] };
    let marchAdded = false;
    let readsWithMarch = 0;
    await page.route(`**/api/dashboards/${DASH_ID}`, (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ...dashboard, selector_bar: [{ dimension_id: "period", default: "MAR" }] }) })
    );
    await page.route("**/api/grid*meta_only*", (route) => {
      if (marchAdded) readsWithMarch++;
      return route.fulfill({ status: 200, contentType: "application/json",
        body: JSON.stringify({ ...gridData, dimensions: marchAdded ? [dimensions[0], withMarch] : dimensions }) });
    });
    await loadAs(page, "dept_head");

    const barPeriod = page.getByRole("toolbar", { name: "Dashboard selectors" }).getByRole("button", { name: "Period", exact: true });
    await expect(barPeriod).toHaveText("January", { timeout: 15_000 });
    if (viewerChose) {
      await barPeriod.click();
      await page.getByRole("option", { name: "January" }).click();
    }
    marchAdded = true;
    await rereadMembers(page);
    if (viewerChose) {
      await expect.poll(() => readsWithMarch).toBeGreaterThan(0);
      await expect(page.getByRole("toolbar", { name: "Dashboard selectors" })).toBeVisible();
      await expect(barPeriod).toHaveText("January");
    } else {
      await expect(barPeriod).toHaveText("March");
    }
  });
}

// While a new selection's cells load, the grid keeps its layout but shows no
// value and takes no input: it read the new cells from the last slice, and
// every input showed an editable 0 until the answer landed (2026-10-23,
// 5 s on the Playground for imported values).
test("selector bar: a grid shows no value and takes no input while the new selection loads", async ({ page }) => {
  await mockApi(page);
  await mockBarDashboard(page);
  // March: a member no widget has read yet, so nothing of it is cached.
  const withMarch = { ...dimensions[1], members: [...dimensions[1].members, { id: "per-mar", code: "MAR", label: "March", parent_code: "Q1" }] };
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/grid*", async (route) => {
    const url = new URL(route.request().url());
    const scope = JSON.parse(url.searchParams.get("scope") ?? "{}") as Record<string, string>;
    if (scope.period === "MAR") {
      await held;
      return route.fulfill({ status: 200, contentType: "application/json",
        body: JSON.stringify({ ...gridData, dimensions: [dimensions[0], withMarch], cells: { "m-cost:ENG:MAR": 18 }, totals: { "m-cost": 18 } }) });
    }
    if (url.searchParams.has("meta_only")) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ...gridData, dimensions: [dimensions[0], withMarch] }) });
    }
    return route.fallback();
  });
  await loadAs(page, "dept_head");

  const grid = page.locator("table").first();
  await expect(grid.locator("tbody input").first()).toHaveValue("200", { timeout: 15_000 });
  const barPeriod = page.getByRole("toolbar", { name: "Dashboard selectors" }).getByRole("button", { name: "Period", exact: true });
  await barPeriod.click();
  await page.getByRole("listbox").getByRole("option", { name: "March" }).click();

  await expect(grid.locator("tbody .mvx-cell--loading").first()).toHaveText("…");
  await expect(grid.locator("tbody input")).toHaveCount(0);
  await expect(grid.locator("tbody")).not.toContainText("0");

  release();
  await expect(grid.locator("tbody input").first()).toHaveValue("18");
  await expect(grid.locator("tbody .mvx-cell--loading")).toHaveCount(0);
});
