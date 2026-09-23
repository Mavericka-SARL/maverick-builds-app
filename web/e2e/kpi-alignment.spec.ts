/**
 * A KPI tile's selector row must not move its number. Three metric_kpi
 * widgets of the same designed height sit side by side: one carrying a
 * selector row above the number, one carrying none at all, one carrying it
 * below. Whatever each tile carries, the label and the value print at the
 * same level across the row — reported live (2026-09-23) as "COST" and
 * "HEADCOUNT" on different levels because the tile with selectors pushed
 * its number down.
 *
 * Self-contained route mocks, like dashboard-context-sync.spec.ts.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "kpi-align-dash";

// Two dimensions so two different tiles can each OWN a selector: the
// dashboard-wide sync dedupes a dimension onto the first widget that asks
// for it, so two tiles sharing "period" would leave the second one bare.
const dimensions = [
  { id: "period", name: "Period", members: [{ id: "per-q1", code: "Q1", label: "Q1 2026" }, { id: "per-q2", code: "Q2", label: "Q2 2026" }] },
  { id: "team",   name: "Team",   members: [{ id: "team-eng", code: "ENG", label: "Engineering" }, { id: "team-ops", code: "OPS", label: "Operations" }] },
];

const metrics = [
  { id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "currency", format_currency: "$", format_decimals: 0, dimension_ids: ["period"] },
  { id: "m-head", name: "headcount", label: "Headcount", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, dimension_ids: ["team"] },
];

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics,
  dimensions,
  departments: [],
  cells: {},
  totals: { "m-cost": 90000, "m-head": 6 },
  access_rules: { dim_members: {}, metrics: {} },
};

const kpi = (id: string, refId: string, x: number, widget_props: Record<string, unknown>) => ({
  id, widget_type: "metric_kpi", ref_id: refId, content: null,
  sort_order: x / 300, pos_x: x, pos_y: 0, size_w: 300, size_h: 140, widget_props,
});

const dashboard = {
  id: DASH_ID,
  name: "KPI Alignment Dashboard",
  tags: [],
  widgets: [
    // Selectors above the number (the default placement).
    kpi("w-top", "m-cost", 0, {}),
    // No selectors at all: a whole-model total.
    kpi("w-none", "m-head", 300, { kpi_context_mode: "total" }),
    // Selectors below the number.
    kpi("w-bottom", "m-head", 600, { selectors_position: "bottom" }),
  ],
};

async function mockKpiDashboard(page: Page) {
  await page.route("**/api/dashboards", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([dashboard]) })
  );
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(dashboard) })
  );
  await page.route("**/api/grid*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(gridData) })
  );
}

async function tops(page: Page, selector: string): Promise<number[]> {
  return page.locator(selector).evaluateAll(els => els.map(e => e.getBoundingClientRect().top));
}

test("a KPI tile's selector row does not move its label or its number", async ({ page }) => {
  await mockApi(page);
  await mockKpiDashboard(page);
  await loadAs(page, "dept_head");

  await expect(page.getByText("KPI Alignment Dashboard")).toBeVisible({ timeout: 15_000 });

  // Two of the three tiles carry a selector row (top and bottom placement);
  // the "total" one carries none.
  await expect(page.locator(".mvx-kpi")).toHaveCount(3);
  await expect(page.locator(".mvx-kpi__selectors")).toHaveCount(2);
  await expect(page.locator(".mvx-kpi__selectors--top")).toHaveCount(1);
  await expect(page.locator(".mvx-kpi__selectors--bottom")).toHaveCount(1);

  // The values are on screen before anything is measured — an unresolved
  // "—" is a different height than a rendered number.
  await expect(page.locator(".mvx-kpi__value").filter({ hasText: "$90,000" })).toHaveCount(1);
  await expect(page.locator(".mvx-kpi__value").filter({ hasText: "6" })).toHaveCount(2);

  const labelTops = await tops(page, ".mvx-kpi__label");
  const valueTops = await tops(page, ".mvx-kpi__value");
  expect(labelTops).toHaveLength(3);
  // One pixel of tolerance for sub-pixel layout, nothing more: the whole
  // point is that the selector row costs the number no vertical offset.
  expect(Math.max(...labelTops) - Math.min(...labelTops)).toBeLessThanOrEqual(1);
  expect(Math.max(...valueTops) - Math.min(...valueTops)).toBeLessThanOrEqual(1);
});

// The reserve opposite the selector row is the row's MEASURED height
// (--kpi-band), not a guess: a tile narrow enough to wrap two dimensions'
// selectors onto two lines still centres its number where a tile without
// selectors centres its own, and grows its row rather than drifting.
test("a wrapped, two-line selector row still leaves the number centred", async ({ page }) => {
  const wide = {
    ...dashboard,
    id: "kpi-wrap-dash",
    name: "KPI Wrap Dashboard",
    // 200px wide: too narrow for "Period: …  Team: …" on one line.
    widgets: [
      { ...kpi("w-wrap", "m-both", 0, {}), size_w: 200 },
      { ...kpi("w-plain", "m-head", 200, { kpi_context_mode: "total" }), size_w: 200 },
    ],
  };
  const both = { ...gridData, metrics: [...metrics, { id: "m-both", name: "both", label: "Cost per head", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, dimension_ids: ["period", "team"] }], totals: { ...gridData.totals, "m-both": 15000 } };
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([wide]) }));
  await page.route("**/api/dashboards/kpi-wrap-dash", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(wide) }));
  await page.route("**/api/grid*", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(both) }));
  await loadAs(page, "dept_head");

  await expect(page.getByText("KPI Wrap Dashboard")).toBeVisible({ timeout: 15_000 });
  await expect(page.locator(".mvx-kpi__value").filter({ hasText: "15,000" })).toHaveCount(1);

  // The narrow tile really did wrap its two selectors onto two lines.
  const bandHeight = await page.locator(".mvx-kpi__selectors").evaluate(el => el.getBoundingClientRect().height);
  expect(bandHeight).toBeGreaterThan(50);

  const labelTops = await tops(page, ".mvx-kpi__label");
  const valueTops = await tops(page, ".mvx-kpi__value");
  expect(Math.max(...labelTops) - Math.min(...labelTops)).toBeLessThanOrEqual(1);
  expect(Math.max(...valueTops) - Math.min(...valueTops)).toBeLessThanOrEqual(1);
});
