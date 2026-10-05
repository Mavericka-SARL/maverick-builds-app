/**
 * Text cells, cleared cells, highlight rules and business-maintained rows
 * (migration 111, found rebuilding a sales target-setting workbook): a text
 * metric's cell shows and writes its note; emptying a number clears it (no
 * value, not 0); a metric's highlight rule tints the cells it holds for —
 * here |var %| over a threshold read from another grid's total; and a
 * planner adds, renames and removes the rows of a dimension the developer
 * marked business-maintained.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "upkeep-dash";
const GRID_ID = "upkeep-grid";
const DIM_ID = "activity";

const metrics = [
  { id: "m-note", name: "act_comment", label: "Comment", is_input: true, agg_rule: "none", format: "text", format_decimals: 0 },
  { id: "m-amount", name: "act_amount", label: "Amount", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
  { id: "m-var", name: "var_pct", label: "Var %", is_input: false, agg_rule: "formula", format: "percentage", format_decimals: 1,
    aggregate_evaluated: true, highlight_rules: [{ abs: true, op: ">", than: "variance_threshold", tone: "negative" }] },
];
const threshold = { id: "m-threshold", name: "variance_threshold", label: "Threshold", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 };

const gridData = {
  revision_id: "rev-1",
  metrics,
  all_metrics: [...metrics, threshold],
  dimensions: [
    { id: DIM_ID, name: "strat_activity", business_maintained: true, display_level: null, members: [
      { id: "a-all", code: "ALL", label: "All Activities" },
      { id: "a-1", code: "STRAT-001", label: "Retail expansion", parent_code: "ALL" },
      { id: "a-2", code: "STRAT-002", label: "Price change", parent_code: "ALL" },
    ] },
  ],
  departments: [],
  cells: {
    "m-note:STRAT-001": 0,
    "m-amount:STRAT-001": 40, "m-amount:STRAT-002": 12,
    "m-var:STRAT-001": 7.5, "m-var:STRAT-002": -1.2, "m-var:ALL": 4,
  },
  texts: { "m-note:STRAT-001": "Retailer confirmed the listing" },
  totals: { "m-amount": 52, "m-var": 4, "m-threshold": 5 },
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = {
  id: DASH_ID,
  name: "Upkeep Dashboard",
  tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 1000, size_h: 360,
    widget_props: { default_view: { rows: [DIM_ID], cols: ["__metrics__"], context: [] } },
  }],
};

type Call = { method: string; url: string; body: Record<string, unknown> | null };

async function mockDashboard(page: Page, calls: Call[]) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
  await page.route("**/api/cells", async (route) => {
    calls.push({ method: "POST", url: "/api/cells", body: route.request().postDataJSON() });
    await route.fulfill(json({ status: "ok" }));
  });
  await page.route(`**/api/dimensions/${DIM_ID}/members**`, async (route) => {
    const req = route.request();
    calls.push({ method: req.method(), url: new URL(req.url()).pathname, body: req.postData() ? req.postDataJSON() : null });
    await route.fulfill(json(req.method() === "POST" ? { id: "a-3", code: "STRAT-003" } : { status: "ok" }));
  });
}

test("notes, clears, highlights and planner-kept rows", async ({ page }) => {
  const calls: Call[] = [];
  await mockApi(page);
  await mockDashboard(page, calls);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Upkeep Dashboard")).toBeVisible({ timeout: 15_000 });

  const table = page.locator("table").first();
  const row1 = table.getByRole("row", { name: /Retail expansion/ });
  const row2 = table.getByRole("row", { name: /Price change/ });

  // The note shows in its cell and is written as text.
  const note = row1.getByLabel("Comment — note");
  await expect(note).toHaveValue("Retailer confirmed the listing", { timeout: 15_000 });
  await row2.getByLabel("Comment — note").fill("Owner: K. Weber");
  await row2.getByLabel("Comment — note").press("Enter");
  await expect.poll(() => calls.filter(c => c.url === "/api/cells").length).toBe(1);
  expect(calls[0].body).toMatchObject({ metric_id: "m-note", text: "Owner: K. Weber", dim_codes: { [DIM_ID]: "STRAT-002" } });

  // Emptying a number clears the cell — no value, not 0.
  const amount = row1.locator("input.mvx-cell-input").nth(1);
  await amount.fill("");
  await amount.press("Enter");
  await expect.poll(() => calls.filter(c => c.url === "/api/cells").length).toBe(2);
  expect(calls[1].body).toMatchObject({ metric_id: "m-amount", clear: true, dim_codes: { [DIM_ID]: "STRAT-001" } });
  expect(calls[1].body).not.toHaveProperty("value");

  // |7.5| > the threshold 5 (another grid's metric, read at its total): red;
  // |-1.2| is within it.
  await expect(row1.locator("td.mvx-cell--tone-negative")).toHaveText("7.5%");
  await expect(row2.locator("td.mvx-cell--tone-negative")).toHaveCount(0);

  // A planner adds a row, renames one and removes one.
  await table.getByRole("button", { name: /Add strat activity/ }).click();
  await page.getByLabel("New strat_activity name").fill("Loyalty programme");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect.poll(() => calls.filter(c => c.method === "POST" && c.url.endsWith("/members")).length).toBe(1);
  expect(calls.find(c => c.method === "POST" && c.url.endsWith("/members"))!.body).toMatchObject({ label: "Loyalty programme", parent_member_id: "a-all" });

  await row2.hover();
  await row2.getByRole("button", { name: "Rename Price change" }).click();
  await page.getByLabel("Rename Price change").fill("Price change Q3");
  await page.getByLabel("Rename Price change").press("Enter");
  await expect.poll(() => calls.filter(c => c.method === "PATCH").length).toBe(1);
  expect(calls.find(c => c.method === "PATCH")).toMatchObject({ url: `/api/dimensions/${DIM_ID}/members/a-2`, body: { label: "Price change Q3" } });

  await row1.hover();
  await row1.getByRole("button", { name: "Remove Retail expansion" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Remove" }).click();
  await expect.poll(() => calls.filter(c => c.method === "DELETE").length).toBe(1);
  expect(calls.find(c => c.method === "DELETE")!.url).toBe(`/api/dimensions/${DIM_ID}/members/a-1`);
});
