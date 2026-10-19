/**
 * A number typed into a grid cell is read the way people write it (owner
 * report, 2026-10-18: editing "$14.69" in place to "$14.80" saved nothing,
 * silently, and "67,40" became 6740). Text that is no number stays in the
 * cell, outlined, and the grid says it was not saved.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "number-dash";
const member = (code: string, label: string) => ({ id: code, code, label });
const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-ly", name: "rev_ly", label: "LY", is_input: true, agg_rule: "sum", format: "currency", format_decimals: 2 }],
  dimensions: [{ id: "product", name: "Product", members: [member("SNACKS", "Snacks"), member("DRINKS", "Drinks")] }],
  departments: [],
  cells: { "m-ly:SNACKS": 14.69, "m-ly:DRINKS": 9.5 },
  totals: { "m-ly": 24.19 },
  access_rules: { dim_members: {}, metrics: {} },
};
const dashboard = {
  id: DASH_ID, name: "Number Dashboard", tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: "number-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 300,
    widget_props: { default_view: { rows: ["product"], cols: ["__metrics__"], context: [] } },
  }],
};

async function setup(page: Page, writes: unknown[]) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
  await page.route("**/api/cells", (route) => {
    writes.push(route.request().postDataJSON());
    return route.fulfill(json({ status: "ok" }));
  });
  await loadAs(page, "dept_head");
}

const cell = (page: Page, row: string) =>
  page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: new RegExp(`^${row}$`) }) }).locator("input").first();

test("currency signs, decimal commas, thousands and (reductions) are read as numbers", async ({ page }) => {
  const writes: { value?: number }[] = [];
  await setup(page, writes);
  const input = cell(page, "Snacks");
  await expect(input).toHaveValue("$14.69", { timeout: 15_000 });

  const cases: [string, number][] = [
    ["$14.80", 14.8], ["67,40", 67.4], ["1.234,5", 1234.5], ["1,234", 1234], ["(2)", -2], ["12 %", 12], ["−3.5", -3.5],
  ];
  for (const [typed, want] of cases) {
    const n = writes.length;
    await input.fill(typed);
    await input.press("Enter");
    await expect.poll(() => writes.length, { message: typed }).toBe(n + 1);
    expect(writes[n].value, typed).toBe(want);
    // The cell is out of editing before the next value is typed.
    await expect(input).not.toHaveClass(/mvx-cell-input--active/);
  }
});

test("text that is not a number stays in the cell and the grid says it was not saved", async ({ page }) => {
  const writes: { value?: number }[] = [];
  await setup(page, writes);
  const input = cell(page, "Drinks");
  await expect(input).toHaveValue("$9.50", { timeout: 15_000 });

  await input.fill("ten");
  await input.press("Enter");
  await expect(page.getByRole("alert")).toContainText("“ten” is not a number, so it was not saved");
  await expect(input).toHaveValue("ten");
  await expect(input).toHaveAttribute("aria-invalid", "true");
  expect(writes).toHaveLength(0);

  await input.fill("10");
  await input.press("Enter");
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0].value).toBe(10);
  await expect(page.getByRole("alert")).toHaveCount(0);
});
