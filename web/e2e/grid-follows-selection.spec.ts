/**
 * A grid whose rows follow the dashboard's selection (widget_props.follow_rows),
 * against stubbed grids: Department rows in one grid, Cabinet rows in another —
 * Cabinet a declared child dimension of Department (parent_dimension_id), its
 * top members naming their department as parent.
 *
 *  - nothing selected: every cabinet;
 *  - a department's row label clicked in the first grid: only its cabinets,
 *    and the grid says it shows what is under the dashboard's selection;
 *  - a cabinet clicked in the following grid selects it for the others and
 *    does not narrow the grid itself;
 *  - another department: its cabinets;
 *  - a records widget following the Cabinet field asks for that department's
 *    cabinets (the parent dimension's selection reaches records too).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "follow-dash";

const dept = { id: "dept", name: "Department", display_level: null, members: [
  { id: "d-fin", code: "FIN", label: "Finance" },
  { id: "d-hr", code: "HR", label: "People" },
] };
const cab = { id: "cab", name: "Cabinet", display_level: null, parent_dimension_id: "dept", members: [
  { id: "c-1", code: "C1", label: "Treasury desk", parent_code: "FIN" },
  { id: "c-2", code: "C2", label: "Ledger desk", parent_code: "FIN" },
  { id: "c-3", code: "C3", label: "Hiring desk", parent_code: "HR" },
] };
const metric = { id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 };
const grid = (dims: unknown[], cells: Record<string, number>) => ({
  scenario: "Working", version: "draft", metrics: [metric], dimensions: dims, all_dimensions: [dept, cab], departments: [], cells, totals: {},
  access_rules: { dim_members: {}, metrics: {} },
});
const GRIDS: Record<string, unknown> = {
  "grid-dept": grid([dept], { "m-cost:FIN": 30, "m-cost:HR": 7 }),
  "grid-cab": grid([cab], { "m-cost:C1": 10, "m-cost:C2": 20, "m-cost:C3": 7 }),
};

// The model's dimensions as the records widget reads them: parents by id.
const DEV_DIMS = [
  { id: "dept", name: "Department", members: dept.members.map(m => ({ ...m, parent_member_id: null })) },
  { id: "cab", name: "Cabinet", parent_dimension_id: "dept", members: cab.members.map(m => ({ id: m.id, code: m.code, label: m.label,
    parent_member_id: dept.members.find(d => d.code === m.parent_code)!.id })) },
];
const STAFF = { id: "form-s", model_id: "model-1", name: "staff", label: "Staff", created_at: "2026-05-01T09:00:00Z",
  fields: [
    { name: "name", label: "Name", type: "text", required: true },
    { name: "cabinet", label: "Cabinet", type: "dimension", required: true, dimension_id: "cab" },
  ],
  permissions: { sync: false, create: true, create_statuses: [] } };

const dashboard = { id: DASH_ID, name: "Departments and cabinets", tags: [], widgets: [
  { id: "w-dept", widget_type: "grid", ref_id: "grid-dept", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 500, size_h: 260,
    widget_props: { default_view: { rows: ["dept"], cols: ["__metrics__"], context: [] } } },
  { id: "w-cab", widget_type: "grid", ref_id: "grid-cab", content: null, sort_order: 1, pos_x: 0, pos_y: 280, size_w: 500, size_h: 300,
    widget_props: { follow_rows: true, default_view: { rows: ["cab"], cols: ["__metrics__"], context: [] } } },
  { id: "w-staff", widget_type: "form", ref_id: "form-s", content: null, sort_order: 2, pos_x: 0, pos_y: 600, size_w: 500, size_h: 260,
    widget_props: { record_follows: ["cabinet"] } },
] };

async function serve(page: Page) {
  const staffQueries: { filters?: unknown[] }[] = [];
  await page.route("**/api/dashboards", (route) => route.fulfill({ json: [dashboard] }));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill({ json: dashboard }));
  await page.route("**/api/grid*", (route) => {
    const id = new URL(route.request().url()).searchParams.get("grid_def_id") ?? "";
    return route.fulfill({ json: GRIDS[id] ?? grid([dept, cab], {}) });
  });
  await page.route((url) => url.pathname === "/api/forms", (route) => route.fulfill({ json: [STAFF] }));
  await page.route((url) => url.pathname === "/api/dimensions", (route) => route.fulfill({ json: DEV_DIMS }));
  await page.route((url) => url.pathname === "/api/forms/form-s/records/query", (route) => {
    staffQueries.push(route.request().postDataJSON());
    return route.fulfill({ json: { kind: "records", table: { id: "form-s", name: "staff", label: "Staff" }, columns: [], records: [], total: 0 } });
  });
  return staffQueries;
}

test("a department clicked in one grid: the other grid's rows are its cabinets", async ({ page }) => {
  await mockApi(page);
  const staffQueries = await serve(page);
  await loadAs(page, "dept_head");
  const cabGrid = page.locator("table").nth(1);
  const rows = cabGrid.locator("tbody");
  await expect(rows.getByText("Hiring desk")).toBeVisible({ timeout: 15_000 });
  await expect(rows.getByText("Treasury desk")).toBeVisible();

  await page.getByTitle("Set Department to Finance for synced widgets").click();
  await expect(rows.getByText("Hiring desk")).toHaveCount(0);
  await expect(rows.getByText("Treasury desk")).toBeVisible();
  await expect(rows.getByText("Ledger desk")).toBeVisible();
  await expect(page.getByText("Shows what is under the dashboard's selection")).toBeVisible();
  await expect.poll(() => JSON.stringify(staffQueries[staffQueries.length - 1]?.filters ?? []))
    .toBe(JSON.stringify([{ field: "cabinet", op: "in", values: ["C1", "C2"] }]));

  // A cabinet clicked here selects it for the others; this grid keeps Finance's cabinets.
  await page.getByTitle("Set Cabinet to Treasury desk for synced widgets").click();
  await expect(rows.getByText("Ledger desk")).toBeVisible();
  await expect.poll(() => JSON.stringify(staffQueries[staffQueries.length - 1]?.filters ?? []))
    .toBe(JSON.stringify([{ field: "cabinet", op: "eq", value: "C1" }]));

  // Another department: its cabinets (the cabinet chosen under Finance no longer applies).
  await page.getByTitle("Set Department to People for synced widgets").click();
  await expect(rows.getByText("Hiring desk")).toBeVisible();
  await expect(rows.getByText("Treasury desk")).toHaveCount(0);
  await expect.poll(() => JSON.stringify(staffQueries[staffQueries.length - 1]?.filters ?? []))
    .toBe(JSON.stringify([{ field: "cabinet", op: "eq", value: "C3" }]));
});

// One dimension at two levels — regions in one grid, countries in the other —
// and a grouping of the cabinets by floor (source_dimension_id/source_property).
test("a region clicked: the countries under it; a floor clicked: the cabinets on it", async ({ page }) => {
  await mockApi(page);
  const geo = { id: "geo", name: "Region", members: [
    { id: "g-eu", code: "EU", label: "Europe" },
    { id: "g-fr", code: "FR", label: "France", parent_code: "EU" },
    { id: "g-de", code: "DE", label: "Germany", parent_code: "EU" },
    { id: "g-am", code: "AM", label: "Americas" },
    { id: "g-us", code: "US", label: "United States", parent_code: "AM" },
  ] };
  const floor = { id: "floor", name: "Floor", display_level: null, source_dimension_id: "cab2", source_property: "floor", members: [
    { id: "f-1", code: "F1", label: "First floor" }, { id: "f-2", code: "F2", label: "Second floor" },
  ] };
  const cab2 = { id: "cab2", name: "Desk", display_level: null, members: [
    { id: "k-1", code: "K1", label: "North desk", properties: { floor: "F1" } },
    { id: "k-2", code: "K2", label: "South desk", properties: { floor: "F2" } },
  ] };
  const all = [geo, floor, cab2];
  const g2 = (dims: unknown[]) => ({ scenario: "Working", version: "draft", metrics: [metric], dimensions: dims, all_dimensions: all, departments: [],
    cells: {}, totals: {}, access_rules: { dim_members: {}, metrics: {} } });
  const grids: Record<string, unknown> = {
    "grid-regions": g2([{ ...geo, display_level: 0 }]),
    "grid-countries": g2([{ ...geo, display_level: -1 }]),
    "grid-floors": g2([floor]),
    "grid-desks": g2([cab2]),
  };
  const view = (dim: string) => ({ default_view: { rows: [dim], cols: ["__metrics__"], context: [] } });
  const dash = { id: "geo-dash", name: "Regions and floors", tags: [], widgets: [
    { id: "w1", widget_type: "grid", ref_id: "grid-regions", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 400, size_h: 200, widget_props: view("geo") },
    { id: "w2", widget_type: "grid", ref_id: "grid-countries", content: null, sort_order: 1, pos_x: 0, pos_y: 220, size_w: 400, size_h: 220, widget_props: { ...view("geo"), follow_rows: true } },
    { id: "w3", widget_type: "grid", ref_id: "grid-floors", content: null, sort_order: 2, pos_x: 0, pos_y: 460, size_w: 400, size_h: 200, widget_props: view("floor") },
    { id: "w4", widget_type: "grid", ref_id: "grid-desks", content: null, sort_order: 3, pos_x: 0, pos_y: 680, size_w: 400, size_h: 200, widget_props: { ...view("cab2"), follow_rows: true } },
  ] };
  await page.route("**/api/dashboards", (route) => route.fulfill({ json: [dash] }));
  await page.route("**/api/dashboards/geo-dash", (route) => route.fulfill({ json: dash }));
  await page.route("**/api/grid*", (route) => {
    const id = new URL(route.request().url()).searchParams.get("grid_def_id") ?? "";
    return route.fulfill({ json: grids[id] ?? g2(all) });
  });
  await loadAs(page, "dept_head");
  const countries = page.locator("table").nth(1).locator("tbody");
  const desks = page.locator("table").nth(3).locator("tbody");
  await expect(countries.getByText("United States")).toBeVisible({ timeout: 15_000 });
  await expect(desks.getByText("South desk")).toBeVisible();

  await page.getByTitle("Set Region to Europe for synced widgets").click();
  await expect(countries.getByText("United States")).toHaveCount(0);
  await expect(countries.getByText("France")).toBeVisible();
  await expect(countries.getByText("Germany")).toBeVisible();
  // A country clicked in the following grid does not narrow it.
  await page.getByTitle("Set Region to France for synced widgets").click();
  await expect(countries.getByText("Germany")).toBeVisible();
  // The Total row of the regions shows every country again (Region has two top members: the selection is cleared).
  await page.getByTitle("Show every Region in synced widgets").first().click();
  await expect(countries.getByText("United States")).toBeVisible();
  await expect(countries.getByText("France")).toBeVisible();

  await page.getByTitle("Set Floor to Second floor for synced widgets").click();
  await expect(desks.getByText("North desk")).toHaveCount(0);
  await expect(desks.getByText("South desk")).toBeVisible();
});
