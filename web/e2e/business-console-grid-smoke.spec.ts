/**
 * Smoke test: the Business Console's planning grid renders calc-metric cells
 * straight from the server's `cells` map (grid()'s calc-value convergence —
 * see IMPLEMENTATION_PLAN.md's P1 "server-side per-intersection
 * calculations" item), not by evaluating a formula client-side. There is no
 * client-side formula evaluator left in this app at all — BusinessConsole.tsx
 * only reads `grid.cells`/`grid.totals`, exactly like it always has for
 * input metrics. This is wiring confidence (does the grid actually render
 * what the server sends), not a test of the calculation engine itself —
 * that's covered exhaustively by internal/gateway and internal/calculation's
 * own Go integration tests (exact numeric fixtures at multiple grains,
 * hidden-member scoping, non-linear formulas).
 *
 * Uses a purpose-built mock (not a live backend + real seed data): CI's e2e
 * job only starts the Vite dev server (see playwright.config.ts's webServer),
 * with no Go gateway or seeded database behind it, matching every other test
 * in this directory except console-smoke.spec.ts (which only asserts on
 * backend-independent shell chrome).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "calc-cells-smoke-dash";
const GRID_ID = "calc-cells-smoke-grid";

// One flat dimension, two leaf members, no hierarchy — every combo below is
// a genuine leaf, so there is no aggregation/rollup ambiguity to reason
// about, unlike mocks.ts's own Department/Region fixture (whose Region
// dimension has real cell data only at the non-leaf "GLOBAL" member).
const dimensions = [
  {
    id: "dept",
    name: "Department",
    members: [
      { id: "dept-mkt", code: "MKT", label: "Marketing" },
      { id: "dept-sales", code: "SALES", label: "Sales" },
    ],
  },
];

// opex_marketing/opex_people are inputs; total_opex and opex_ratio are calc
// metrics whose formula strings are carried in the response (as the real
// grid() response still does) but never evaluated client-side — their
// `cells` entries below are what actually renders, exactly as the converged
// backend would supply them (see internal/gateway/handler.go's grid()).
const metrics = [
  { id: "m-mkt", name: "opex_marketing", label: "Marketing Spend", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
  { id: "m-ppl", name: "opex_people", label: "People Spend", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
  { id: "m-tot", name: "total_opex", label: "Total OPEX", is_input: false, formula: "=opex_marketing+opex_people", agg_rule: "sum", format: "number", format_decimals: 0 },
  { id: "m-ratio", name: "opex_ratio", label: "People-to-Marketing Ratio", is_input: false, formula: "=ROUND(opex_people/opex_marketing*100,1)", agg_rule: "sum", format: "number", format_decimals: 1 },
];

// Inputs: MKT marketing=240000/people=310000, SALES marketing=90000/people=420000.
// Calc cells below are hand-computed from those same inputs (matching what
// grid()'s calc block would itself have produced from runtime.calc_result),
// not evaluated by anything in this test or in BusinessConsole.tsx.
const cells: Record<string, number> = {
  "m-mkt:MKT": 240000,
  "m-ppl:MKT": 310000,
  "m-mkt:SALES": 90000,
  "m-ppl:SALES": 420000,
  "m-tot:MKT": 550000, // 240000+310000
  "m-tot:SALES": 510000, // 90000+420000
  "m-ratio:MKT": 129.2, // ROUND(310000/240000*100,1)
  "m-ratio:SALES": 466.7, // ROUND(420000/90000*100,1)
};

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics,
  dimensions,
  departments: [],
  cells,
  totals: { "m-mkt": 330000, "m-ppl": 730000, "m-tot": 1060000, "m-ratio": 595.9 },
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = {
  id: DASH_ID,
  name: "Calc Cells Smoke Dashboard",
  tags: [],
  widgets: [
    { id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, col_start: 1, col_span: 12 },
  ],
};

async function mockCalcCellsSmokeGrid(page: Page) {
  // Registered after mockApi so these more specific routes win (Playwright
  // checks routes most-recently-registered-first) while everything else
  // (dev personas, /api/demo, ...) still falls through to mockApi's own
  // generic fixture.
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

test("Business Console grid: calc cells render server-precomputed values verbatim", async ({ page }) => {
  await mockApi(page);
  await mockCalcCellsSmokeGrid(page);
  await loadAs(page, "dept_head");

  await expect(page.getByText("Calc Cells Smoke Dashboard")).toBeVisible({ timeout: 15_000 });

  const table = page.locator("table").first();
  const cellFor = (metricPattern: RegExp, colIndex: number) =>
    table.getByRole("row", { name: metricPattern }).getByRole("cell").nth(colIndex);

  // Default pivot: metrics in rows, the sole dimension (Department) in
  // columns, alphabetically-coded members MKT then SALES -> columns
  // [label, Marketing, Sales] (no hierarchy, so no extra group-total column).
  const MKT_COL = 1;
  const SALES_COL = 2;

  await expect(cellFor(/Total OPEX/, MKT_COL)).toHaveText("550,000");
  await expect(cellFor(/Total OPEX/, SALES_COL)).toHaveText("510,000");
  await expect(cellFor(/People-to-Marketing Ratio/, MKT_COL)).toHaveText("129.2");
  await expect(cellFor(/People-to-Marketing Ratio/, SALES_COL)).toHaveText("466.7");
});

// ── The flat grid's Total row ────────────────────────────────────────────────
// A formula/rate metric's total is the server's value, never the sum of the
// rows (a sum of ratios); a total the server withheld from a restricted
// viewer is "—", never the partial sum of the rows they can see.

const TOTAL_DASH_ID = "total-row-dash";
const TOTAL_GRID_ID = "total-row-grid";

function totalRowGrid(totals: Record<string, number>, withheld: string[]) {
  return {
    scenario: "Working",
    version: "draft",
    metrics: [
      { id: "m-share", name: "share", label: "Share", is_input: false, formula: "=a/b", agg_rule: "formula", format: "number", format_decimals: 2 },
      { id: "m-sum", name: "amount", label: "Amount", is_input: false, formula: "=a", agg_rule: "sum", format: "number", format_decimals: 0 },
    ],
    dimensions: [
      { id: "region", name: "Region", members: [
        { id: "r-de", code: "DE", label: "Germany" },
        { id: "r-uk", code: "UK", label: "United Kingdom" },
      ] },
    ],
    departments: [],
    cells: { "m-share:DE": 0.4, "m-share:UK": 0.35, "m-sum:UK": 5 },
    totals,
    withheld,
    access_rules: { dim_members: {}, metrics: {} },
  };
}

async function mockTotalRowGrid(page: Page, grid: object) {
  const dash = {
    id: TOTAL_DASH_ID,
    name: "Total Row Dashboard",
    tags: [],
    widgets: [{
      id: "w-total", widget_type: "grid", ref_id: TOTAL_GRID_ID, content: null, sort_order: 0, col_start: 1, col_span: 12,
      widget_props: { default_view: { rows: ["region"], cols: ["__metrics__"], context: [] } },
    }],
  };
  await page.route("**/api/dashboards", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([dash]) })
  );
  await page.route(`**/api/dashboards/${TOTAL_DASH_ID}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(dash) })
  );
  await page.route("**/api/grid*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(grid) })
  );
}

test("Business Console grid: the Total row of a formula metric is the server's total, never a sum of ratios", async ({ page }) => {
  await mockApi(page);
  await mockTotalRowGrid(page, totalRowGrid({ "m-share": 0.38, "m-sum": 12 }, []));
  await loadAs(page, "dept_head");
  await expect(page.getByText("Total Row Dashboard")).toBeVisible({ timeout: 15_000 });
  const total = page.locator("table").first().getByRole("row", { name: /^Total/ });
  await expect(total.getByRole("cell").nth(1)).toHaveText("0.38");
  await expect(total.getByRole("cell").nth(2)).toHaveText("5");
});

test("Business Console grid: a withheld total renders as a dash, never the visible rows' sum", async ({ page }) => {
  await mockApi(page);
  await mockTotalRowGrid(page, totalRowGrid({}, ["m-share", "m-sum:DE", "m-sum"]));
  await loadAs(page, "dept_head");
  await expect(page.getByText("Total Row Dashboard")).toBeVisible({ timeout: 15_000 });
  const total = page.locator("table").first().getByRole("row", { name: /^Total/ });
  await expect(total.getByRole("cell").nth(1)).toHaveText("—");
  await expect(total.getByRole("cell").nth(2)).toHaveText("—");
});

// ── Parents two or more levels up reduce FLAT over their leaves ──────────────
// agg_rule average is the mean of every leaf under the parent that has a
// value (a leaf with none is left out, never a 0) and count is the number of
// leaves with a non-zero value (rollup.CombineAgg's rule) — never a mean of
// the children's means or a count of non-zero children. Region: Global > {World > {EMEA > {UK 2, DE 3, FR —},
// US 4}, APAC > {JP 10}}, the fixture of docs/OBSERVATIONS.md's "The grid's
// parent rows differ from chart-data for member-metadata metrics".
//                 flat (now)          level by level (before)
//   average  EMEA 2.50 World 3.00 Global 4.75 | 1.67  2.83  6.42
//   count    EMEA 2    World 3    Global 4    | 2     2     2
//   sum      EMEA 5    World 9    Global 19   | (the same)

const FLAT_DASH_ID = "flat-parent-dash";
const FLAT_GRID_ID = "flat-parent-grid";

async function mockGridDashboard(page: Page, dashId: string, gridId: string, name: string, defaultView: object, grid: object) {
  const dash = {
    id: dashId,
    name,
    tags: [],
    widgets: [{
      id: `w-${dashId}`, widget_type: "grid", ref_id: gridId, content: null, sort_order: 0, col_start: 1, col_span: 12,
      widget_props: { default_view: defaultView },
    }],
  };
  await page.route("**/api/dashboards", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([dash]) })
  );
  await page.route(`**/api/dashboards/${dashId}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(dash) })
  );
  await page.route("**/api/grid*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(grid) })
  );
}

const regionHierarchy = {
  id: "region", name: "Region", display_level: null, members: [
    { id: "r-global", code: "GLOBAL", label: "Global" },
    { id: "r-world", code: "WORLD", label: "World", parent_code: "GLOBAL" },
    { id: "r-apac", code: "APAC", label: "Asia Pacific", parent_code: "GLOBAL" },
    { id: "r-emea", code: "EMEA", label: "Europe", parent_code: "WORLD" },
    { id: "r-us", code: "US", label: "United States", parent_code: "WORLD" },
    { id: "r-uk", code: "UK", label: "United Kingdom", parent_code: "EMEA" },
    { id: "r-de", code: "DE", label: "Germany", parent_code: "EMEA" },
    { id: "r-fr", code: "FR", label: "France", parent_code: "EMEA" },
    { id: "r-jp", code: "JP", label: "Japan", parent_code: "APAC" },
  ],
};

const flatParentGrid = {
  scenario: "Working",
  version: "draft",
  metrics: [
    { id: "m-avg", name: "wavg", label: "Avg", is_input: true, agg_rule: "average", format: "number", format_decimals: 2, dimension_ids: ["region"] },
    { id: "m-cnt", name: "wcnt", label: "Cnt", is_input: true, agg_rule: "count", format: "number", format_decimals: 0, dimension_ids: ["region"] },
    { id: "m-amt", name: "wsum", label: "Amt", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, dimension_ids: ["region"] },
  ],
  dimensions: [regionHierarchy],
  departments: [],
  cells: {
    "m-avg:UK": 2, "m-avg:DE": 3, "m-avg:US": 4, "m-avg:JP": 10,
    "m-cnt:UK": 2, "m-cnt:DE": 3, "m-cnt:US": 4, "m-cnt:JP": 10,
    "m-amt:UK": 2, "m-amt:DE": 3, "m-amt:US": 4, "m-amt:JP": 10,
  },
  totals: { "m-avg": 4.75, "m-cnt": 4, "m-amt": 19 },
  access_rules: { dim_members: {}, metrics: {} },
};

test("Business Console grid: a parent two levels up averages and counts its leaves flat", async ({ page }) => {
  await mockApi(page);
  await mockGridDashboard(page, FLAT_DASH_ID, FLAT_GRID_ID, "Flat Parent Dashboard",
    { rows: ["region"], cols: ["__metrics__"], context: [] }, flatParentGrid);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Flat Parent Dashboard")).toBeVisible({ timeout: 15_000 });
  const table = page.locator("table").first();
  // Hierarchy rows are labelled "└Europe"; a parent's cells are read-only
  // text, a leaf's are inputs (an empty input is a leaf with no value).
  const row = (label: string) => table.getByRole("row", { name: new RegExp(`^└?${label} `) });
  const expectRow = async (label: string, avg: string, cnt: string, amt: string) => {
    const cells = row(label).getByRole("cell");
    await expect(cells.nth(1), `${label} average`).toHaveText(avg);
    await expect(cells.nth(2), `${label} count`).toHaveText(cnt);
    await expect(cells.nth(3), `${label} sum`).toHaveText(amt);
  };
  const expectLeaf = async (label: string, avg: string, cnt: string, amt: string) => {
    const inputs = row(label).getByRole("textbox");
    await expect(inputs.nth(0), `${label} average`).toHaveValue(avg);
    await expect(inputs.nth(1), `${label} count`).toHaveValue(cnt);
    await expect(inputs.nth(2), `${label} sum`).toHaveValue(amt);
  };
  await expectRow("Europe", "2.50", "2", "5");
  await expectRow("World", "3.00", "3", "9");
  await expectRow("Global", "4.75", "4", "19");
  await expectRow("Asia Pacific", "10.00", "1", "10");
  // A leaf with no value: no average, a count and a sum of 0 (as before);
  // a leaf's own value is untouched.
  await expectLeaf("France", "", "0", "0");
  await expectLeaf("Germany", "3.00", "3", "3");
});

// The flat Total row reduces the same way: it is the server's total for
// sum/average/count (mean of the leaves with a value, number of leaves with
// a value), never the sum of the rows. And across the periods of a time
// dimension it combines each leaf period flat first, then the periods by the
// metric's time_summary (here "last": a closing balance).
const TIME_DASH_ID = "flat-total-dash";
const TIME_GRID_ID = "flat-total-grid";

const flatTotalGrid = {
  scenario: "Working",
  version: "draft",
  metrics: [
    { id: "m-bal", name: "balance", label: "Balance", is_input: true, agg_rule: "sum", time_summary: "last", format: "number", format_decimals: 0, dimension_ids: ["region2", "month"] },
    { id: "m-avg2", name: "price", label: "Price", is_input: true, agg_rule: "average", time_summary: "average", format: "number", format_decimals: 1, dimension_ids: ["region2", "month"] },
  ],
  dimensions: [
    { id: "region2", name: "Country", display_level: null, members: [
      { id: "c-de", code: "DE", label: "Germany" },
      { id: "c-fr", code: "FR", label: "France" },
      { id: "c-uk", code: "UK", label: "United Kingdom" },
    ] },
    { id: "month", name: "Month", dimension_type: "time", time_granularity: "month", display_level: null, members: [
      { id: "t-q1", code: "Q1", label: "Q1" },
      { id: "t-jan", code: "M01", label: "Jan", parent_code: "Q1", time_index: 0, period_start: "2026-01-01" },
      { id: "t-feb", code: "M02", label: "Feb", parent_code: "Q1", time_index: 1, period_start: "2026-02-01" },
    ] },
  ],
  departments: [],
  cells: {
    "m-bal:DE:M01": 10, "m-bal:DE:M02": 12, "m-bal:UK:M01": 5,
    "m-avg2:DE:M01": 2, "m-avg2:UK:M01": 4, "m-avg2:DE:M02": 6,
  },
  // balance: Jan 15, Feb 12 -> last 12; price: Jan mean(2,4)=3, Feb 6 -> average 4.5.
  totals: { "m-bal": 12, "m-avg2": 4.5 },
  access_rules: { dim_members: {}, metrics: {} },
};

async function expectTimeTotals(page: Page, metric: string, jan: string, feb: string, q1: string, rowsQ1: Record<string, string>) {
  const table = page.locator("table").first();
  const row = (label: string) => table.getByRole("row", { name: new RegExp(`^${label} `) });
  // Columns: [Country, Jan, Feb, Q1] (a group's Total column follows its leaves).
  const total = table.getByRole("row", { name: /^Total/ }).getByRole("cell");
  await expect(total.nth(1), `${metric} Jan total`).toHaveText(jan);
  await expect(total.nth(2), `${metric} Feb total`).toHaveText(feb);
  await expect(total.nth(3), `${metric} Q1 total`).toHaveText(q1);
  for (const [label, v] of Object.entries(rowsQ1)) {
    await expect(row(label).getByRole("cell").nth(3), `${metric} ${label} Q1`).toHaveText(v);
  }
}

test("Business Console grid: the Total row reduces flat, and an aggregate period by time_summary", async ({ page }) => {
  await mockApi(page);
  await mockGridDashboard(page, TIME_DASH_ID, TIME_GRID_ID, "Flat Total Dashboard",
    { rows: ["region2"], cols: ["month"], context: ["__metrics__"] }, flatTotalGrid);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Flat Total Dashboard")).toBeVisible({ timeout: 15_000 });
  // Balance (sum, time_summary last): level by level the Q1 total was 27.
  await expectTimeTotals(page, "balance", "15", "12", "12", { Germany: "12", "United Kingdom": "5", France: "0" });
  // Price (average, time_summary average): Jan's total is mean(2, 4) = 3, not
  // the rows' sum 6; Q1 is the average of the periods, mean(3, 6) = 4.5.
  await page.getByRole("combobox", { name: "Metric context" }).selectOption("m-avg2");
  await expectTimeTotals(page, "price", "3.0", "6.0", "4.5", { Germany: "4.0", "United Kingdom": "4.0", France: "—" });
});

// A calculated metric's leaf is its own server value, whatever its agg_rule:
// a count leaf once rendered as 1 (one value "counted"), not its value.
// Count counts the leaves whose value is not 0, as the server does
// (rollup.CombineAgg): the scheduler writes a 0 row wherever a formula saw
// no inputs (DE, FR here), and a typed 0 is not counted either. And a
// pure-ratio average (aggregate_evaluated) is its formula at the aggregate,
// so a parent is the server's row or blank, and the top is the server's
// total — never the mean of the leaf cells.
//                          UK  DE  FR  US  JP | EMEA World APAC Global
//   ccnt (calc, count)     4   0   0   8   14 |  1    2     1    3
//   izc  (input, count)    2   0   —   5   —  |  1    2     0    2
//   pr   (pure-ratio avg)  2   4   —   6   8  |  —    —     —    9 (server)
const CALC_DASH_ID = "calc-count-dash";
const CALC_GRID_ID = "calc-count-grid";

const calcCountGrid = {
  scenario: "Working",
  version: "draft",
  metrics: [
    { id: "m-ccnt", name: "ccnt", label: "CCnt", is_input: false, formula: "iavg * 2", agg_rule: "count", format: "number", format_decimals: 0, dimension_ids: ["region"] },
    { id: "m-izc", name: "izc", label: "IZc", is_input: true, agg_rule: "count", format: "number", format_decimals: 0, dimension_ids: ["region"] },
    { id: "m-pr", name: "pr", label: "PR", is_input: false, formula: "a / b", agg_rule: "average", aggregate_evaluated: true, format: "number", format_decimals: 2, dimension_ids: ["region"] },
  ],
  dimensions: [regionHierarchy],
  departments: [],
  cells: {
    "m-ccnt:UK": 4, "m-ccnt:DE": 0, "m-ccnt:FR": 0, "m-ccnt:US": 8, "m-ccnt:JP": 14,
    "m-izc:UK": 2, "m-izc:DE": 0, "m-izc:US": 5,
    "m-pr:UK": 2, "m-pr:DE": 4, "m-pr:US": 6, "m-pr:JP": 8,
  },
  totals: { "m-ccnt": 3, "m-izc": 2, "m-pr": 9 },
  access_rules: { dim_members: {}, metrics: {} },
};

test("Business Console grid: a calculated leaf is its own value; count skips zeros; a pure ratio is the server's", async ({ page }) => {
  await mockApi(page);
  await mockGridDashboard(page, CALC_DASH_ID, CALC_GRID_ID, "Calc Count Dashboard",
    { rows: ["region"], cols: ["__metrics__"], context: [] }, calcCountGrid);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Calc Count Dashboard")).toBeVisible({ timeout: 15_000 });
  const table = page.locator("table").first();
  const row = (label: string) => table.getByRole("row", { name: new RegExp(`^└?${label} `) });
  // Columns: [Region, CCnt, IZc, PR]; calc cells are read-only text.
  const expectCalc = async (label: string, ccnt: string, pr: string) => {
    const cells = row(label).getByRole("cell");
    await expect(cells.nth(1), `${label} ccnt`).toHaveText(ccnt);
    await expect(cells.nth(3), `${label} pr`).toHaveText(pr);
  };
  await expectCalc("United Kingdom", "4", "2.00");
  await expectCalc("Germany", "0", "4.00");
  await expectCalc("United States", "8", "6.00");
  await expectCalc("Japan", "14", "8.00");
  await expectCalc("Europe", "1", "—");
  await expectCalc("World", "2", "—");
  await expectCalc("Asia Pacific", "1", "—");
  await expectCalc("Global", "3", "9.00");
  const izc = async (label: string, v: string) =>
    expect(row(label).getByRole("cell").nth(2), `${label} izc`).toHaveText(v);
  await izc("Europe", "1");
  await izc("World", "2");
  await izc("Global", "2");
});
