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
