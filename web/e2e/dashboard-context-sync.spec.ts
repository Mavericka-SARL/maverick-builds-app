/**
 * Smoke test for HierarchicalMemberSelect + the cross-widget context-sync
 * mechanism (DashboardContextSyncProvider/useWidgetContextSync). Three
 * `grid` widgets on one dashboard, all pointed at the same grid def (so
 * they share the same "period" context dimension) — two opted into
 * `widget_props.sync_context`, one not. Uses grid widgets rather than
 * chart widgets purely to avoid also mocking /api/chart-data; the sync
 * mechanism itself is widget-type-agnostic (same hook, same provider).
 *
 * Mirrors business-console-grid-smoke.spec.ts's self-contained route-mock
 * style, not mocks.ts's shared fixture (that fixture's own Department/
 * Region dims aren't used here).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "sync-smoke-dash";
const GRID_ID = "sync-smoke-grid";

// "period" is a real hierarchy (Q1 -> Jan/Feb) so this also smoke-tests
// HierarchicalMemberSelect's search-filter ancestor-preservation, not just
// sync. "dept" is flat and lands in Cols by default (PlanningGrid's own
// fallback: first dim -> Cols, rest -> Context), leaving "period" as the
// one and only context selector each grid widget renders.
const dimensions = [
  {
    id: "dept", name: "Department",
    members: [{ id: "dept-a", code: "ENG", label: "Engineering" }],
  },
  {
    id: "period", name: "Period",
    members: [
      { id: "per-q1", code: "Q1", label: "Q1" },
      { id: "per-jan", code: "JAN", label: "January", parent_code: "Q1" },
      { id: "per-feb", code: "FEB", label: "February", parent_code: "Q1" },
    ],
  },
];

const metrics = [
  { id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
];

const cells: Record<string, number> = {
  "m-cost:ENG:JAN": 100,
  "m-cost:ENG:FEB": 200,
};

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics,
  dimensions,
  departments: [],
  cells,
  totals: { "m-cost": 300 },
  access_rules: { dim_members: {}, metrics: {} },
};

// Widget A/B synced (the default — sync_context absent), C explicitly
// opted out — laid out left-to-right so DOM order (and thus Playwright's
// .nth()) matches A, B, C.
const dashboard = {
  id: DASH_ID,
  name: "Context Sync Smoke Dashboard",
  tags: [],
  widgets: [
    { id: "w-a", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 400, size_h: 300, widget_props: { sync_context: true } },
    { id: "w-b", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 1, pos_x: 400, pos_y: 0, size_w: 400, size_h: 300 },
    { id: "w-c", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 2, pos_x: 800, pos_y: 0, size_w: 400, size_h: 300, widget_props: { sync_context: false } },
    // D: synced, but laid out with PERIOD on its columns (and dept as its
    // context) — so its column headers are Q1 / January / February. Clicking
    // one of them is the click-to-select path for the shared Period value.
    { id: "w-d", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 3, pos_x: 0, pos_y: 300, size_w: 400, size_h: 300,
      widget_props: { sync_context: true, default_view: { rows: ["__metrics__"], cols: ["period"], context: ["dept"] } } },
    // E: NOT synced, period on columns. Its own selectors ignore the shared
    // context, but a click on one of its column headers is the user's explicit
    // choice and must still move the shared selector (reported live: "click in
    // region doesn't sync" on an unsynced chart).
    { id: "w-e", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 4, pos_x: 400, pos_y: 300, size_w: 400, size_h: 300,
      widget_props: { sync_context: false, default_view: { rows: ["__metrics__"], cols: ["period"], context: ["dept"] } } },
  ],
};

async function mockSyncSmokeDashboard(page: Page) {
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

test("dashboard context sync: synced widgets stay in lockstep, unsynced widgets stay independent", async ({ page }) => {
  await mockApi(page);
  await mockSyncSmokeDashboard(page);
  await loadAs(page, "dept_head");

  await expect(page.getByText("Context Sync Smoke Dashboard")).toBeVisible({ timeout: 15_000 });

  // Selector DEDUP: the synced pair (A, B) shares ONE Period selector
  // (A owns it, tree order); C opted out and keeps its own. The same
  // selector must never appear once per widget (reported live).
  const periodTriggers = page.getByLabel("Period context");
  await expect(periodTriggers).toHaveCount(2);
  // Hierarchy-aware default leaf: within Q1's children, "FEB" sorts before
  // "JAN" alphabetically by code (matching PlanningGrid's own pre-existing,
  // deliberately-alphabetical-not-chronological tree sort) — same default
  // everywhere before any interaction.
  await expect(periodTriggers.nth(0)).toHaveText("February");
  await expect(periodTriggers.nth(1)).toHaveText("February");
  const cellOf = (widgetIdx: number) => page.locator("table").nth(widgetIdx).locator("tbody input").first();
  await expect(cellOf(0)).toHaveValue("200");
  await expect(cellOf(1)).toHaveValue("200");
  await expect(cellOf(2)).toHaveValue("200");

  // Change the shared selector (owned by A) to January.
  await periodTriggers.nth(0).click();
  await page.getByRole("listbox").getByRole("option", { name: "January" }).click();

  // Widget B has NO selector of its own but follows the shared context —
  // its data flips to January's value; opted-out C stays on February.
  await expect(periodTriggers.nth(0)).toHaveText("January");
  await expect(periodTriggers.nth(1)).toHaveText("February");
  await expect(cellOf(0)).toHaveValue("100");
  await expect(cellOf(1)).toHaveValue("100");
  await expect(cellOf(2)).toHaveValue("200");
});

test("HierarchicalMemberSelect search filters the tree while keeping ancestors visible", async ({ page }) => {
  await mockApi(page);
  await mockSyncSmokeDashboard(page);
  await loadAs(page, "dept_head");

  await expect(page.getByText("Context Sync Smoke Dashboard")).toBeVisible({ timeout: 15_000 });

  await page.getByLabel("Period context").first().click();
  const listbox = page.getByRole("listbox");
  await expect(listbox.getByRole("option")).toHaveCount(3); // Q1, January, February

  await page.getByPlaceholder("Search…").fill("feb");
  // "January" (a non-matching sibling) is hidden; "Q1" (the matching
  // ancestor) stays visible for context; "February" (the match) stays.
  await expect(listbox.getByRole("option")).toHaveCount(2);
  await expect(listbox.getByRole("option", { name: "Q1" })).toBeVisible();
  await expect(listbox.getByRole("option", { name: "February" })).toBeVisible();
  await expect(listbox.getByRole("option", { name: "January" })).not.toBeVisible();
});

test("HierarchicalMemberSelect supports keyboard navigation", async ({ page }) => {
  await mockApi(page);
  await mockSyncSmokeDashboard(page);
  await loadAs(page, "dept_head");

  await expect(page.getByText("Context Sync Smoke Dashboard")).toBeVisible({ timeout: 15_000 });

  const trigger = page.getByLabel("Period context").first();
  await expect(trigger).toHaveText("February"); // default: "FEB" sorts before "JAN" (alphabetical by code)
  await trigger.click();

  // Tree order: Q1, February, January — activeCode starts on the current
  // value (February); ArrowDown once moves to January.
  await expect(page.locator(".mvx-hier-select__option--active")).toHaveText("February");
  await page.keyboard.press("ArrowDown");
  await expect(page.locator(".mvx-hier-select__option--active")).toHaveText("January");
  await page.keyboard.press("Enter");

  await expect(page.getByRole("listbox")).not.toBeVisible();
  await expect(trigger).toHaveText("January");
});

// Click-to-select: a member label on a row/column axis of one synced widget
// pushes that member into the shared context, so every other synced
// widget's selector for that dimension follows (reported as a wish live,
// 2026-09-10: "click Germany in the grid, all relevant selectors change").
test("clicking a column member label in a synced grid moves the shared selector", async ({ page }) => {
  await mockApi(page);
  await mockSyncSmokeDashboard(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Context Sync Smoke Dashboard")).toBeVisible({ timeout: 15_000 });

  const periodTriggers = page.getByLabel("Period context");
  await expect(periodTriggers.nth(0)).toHaveText("February");
  const cellOf = (widgetIdx: number) => page.locator("table").nth(widgetIdx).locator("tbody input").first();
  await expect(cellOf(0)).toHaveValue("200");

  // Widget D's "January" column header carries the pickable affordance…
  const january = page.locator("table").nth(3).locator("th.mvx-header-cell--pickable", { hasText: "January" });
  await expect(january).toHaveAttribute("title", /Set Period to January/);
  await january.click();

  // …and the synced pair follows; opted-out C does not.
  await expect(periodTriggers.nth(0)).toHaveText("January");
  await expect(periodTriggers.nth(1)).toHaveText("February");
  await expect(cellOf(0)).toHaveValue("100");
  await expect(cellOf(1)).toHaveValue("100");
  await expect(cellOf(2)).toHaveValue("200");
});

// Editable cells must be recognisable at a glance (Anaplan/Pigment/Board
// convention): a tinted, bordered input, plus a legend naming the roles.
test("planning grid tints editable cells and shows a cell legend", async ({ page }) => {
  await mockApi(page);
  await mockSyncSmokeDashboard(page);
  await loadAs(page, "dept_head");
  const input = page.locator("table").first().locator("tbody input").first();
  await expect(input).toBeVisible({ timeout: 15_000 });
  await expect(input).toHaveClass(/mvx-cell-input/);
  const bg = await input.evaluate((el) => getComputedStyle(el).backgroundColor);
  const border = await input.evaluate((el) => getComputedStyle(el).borderTopColor);
  expect(bg).toBe("rgb(244, 248, 255)");
  expect(border).toBe("rgb(207, 220, 247)");
  const legend = page.locator(".mvx-grid-legend").first();
  await expect(legend.getByText("Editable")).toBeVisible();
  await expect(legend.getByText(/Click a row or column label/)).toBeVisible();
});

test("a click in an UNSYNCED widget still moves the shared selector", async ({ page }) => {
  await mockApi(page);
  await mockSyncSmokeDashboard(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Context Sync Smoke Dashboard")).toBeVisible({ timeout: 15_000 });
  const periodTriggers = page.getByLabel("Period context");
  await expect(periodTriggers.nth(0)).toHaveText("February");
  const january = page.locator("table").nth(4).locator("th.mvx-header-cell--pickable", { hasText: "January" });
  await january.click();
  await expect(periodTriggers.nth(0)).toHaveText("January");
  await expect(page.locator("table").nth(0).locator("tbody input").first()).toHaveValue("100");
});
