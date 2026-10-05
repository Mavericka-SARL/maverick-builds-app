/**
 * A grid widget's widget_props.show_members lists the members an axis shows,
 * flat and in that order — a workforce table with FY, Q1, Q2 and H2 as its
 * columns instead of the year's four header rows — and a total among them
 * still adds up its leaves. Its saved layout here leaves "context" out, as
 * the AI Developer saved one: the widget used to fail with "defaultView.context
 * is not iterable" (found rebuilding an HR planning workbook, 2026-10-05).
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "members-shown-dash";
const GRID_ID = "members-shown-grid";

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-hires", name: "hires", label: "Hire Events", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 }],
  dimensions: [
    {
      id: "month", name: "Month", members: [
        { id: "fy", code: "FY", label: "FY 2027" },
        { id: "h1", code: "H1", label: "H1 2027", parent_code: "FY" },
        { id: "h2", code: "H2", label: "H2 2027", parent_code: "FY" },
        { id: "q1", code: "Q1", label: "Q1 2027", parent_code: "H1" },
        { id: "q2", code: "Q2", label: "Q2 2027", parent_code: "H1" },
        { id: "q3", code: "Q3", label: "Q3 2027", parent_code: "H2" },
        { id: "q4", code: "Q4", label: "Q4 2027", parent_code: "H2" },
      ],
    },
  ],
  departments: [],
  cells: { "m-hires:Q1": 2, "m-hires:Q2": 3, "m-hires:Q3": 1, "m-hires:Q4": 4 },
  totals: { "m-hires": 10 },
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = {
  id: DASH_ID,
  name: "Members Shown Dashboard",
  tags: [],
  widgets: [
    {
      id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 800, size_h: 260,
      widget_props: {
        default_view: { rows: ["__metrics__"], cols: ["month"] },
        show_members: { month: ["FY", "Q1", "Q2", "H2"] },
      },
    },
  ],
};

async function mockDashboard(page: Page) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
}

test("a grid widget shows only the chosen members, in order, with totals from their leaves", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Members Shown Dashboard")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("couldn't render")).toHaveCount(0);

  const table = page.locator("table").first();
  const headers = (await table.locator("thead th").allTextContents()).map((t) => t.trim()).filter(Boolean);
  expect(headers.slice(1)).toEqual(["FY 2027", "Q1 2027", "Q2 2027", "H2 2027"]);
  await expect(table.getByText("H1 2027")).toHaveCount(0);
  await expect(table.getByText("Q3 2027")).toHaveCount(0);

  // FY and H2 are totals of their quarters; Q1 and Q2 are the typed leaves.
  const row = table.locator("tbody tr").first();
  const cells = row.locator("td");
  await expect(cells.nth(1)).toHaveText("10");
  await expect(cells.nth(2).locator("input")).toHaveValue("2");
  await expect(cells.nth(3).locator("input")).toHaveValue("3");
  await expect(cells.nth(4)).toHaveText("5");
});
