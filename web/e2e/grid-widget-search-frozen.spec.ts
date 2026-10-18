/**
 * Grid widgets (owner request, 2026-10-18):
 *  - widget_props.hide_search leaves out "Find a row or column…" (a one-row
 *    setup grid needs none); other grids keep it;
 *  - frozen panes: a grid scrolls inside its widget, and its header rows and
 *    row-label column stay where they are.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "frozen-dash";

const member = (code: string, label: string, parent_code?: string) => ({ id: code, code, label, ...(parent_code ? { parent_code } : {}) });
const products = Array.from({ length: 30 }, (_, i) => member(`P${i + 1}`, `Product ${i + 1}`));
const months = [member("FY", "FY 2026"), ...Array.from({ length: 12 }, (_, i) => member(`M${i + 1}`, `Month ${i + 1}`, "FY"))];
const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-pct", name: "plan_pct", label: "Plan %", is_input: true, agg_rule: "average", format: "number", format_decimals: 0 }],
  dimensions: [{ id: "product", name: "Product", members: products }, { id: "month", name: "Month", members: months }],
  departments: [],
  cells: Object.fromEntries(products.flatMap((p, pi) => months.slice(1).map((m, mi) => [`m-pct:${p.code}:${m.code}`, pi * 100 + mi]))),
  totals: {},
  access_rules: { dim_members: {}, metrics: {} },
};

const widget = (id: string, ref: string, pos_y: number, size_h: number, props: Record<string, unknown>) => ({
  id, widget_type: "grid", ref_id: ref, content: null, sort_order: pos_y, pos_x: 0, pos_y, size_w: 900, size_h, widget_props: props,
});
const dashboard = {
  id: DASH_ID,
  name: "Frozen Dashboard",
  tags: [],
  widgets: [
    widget("w-setup", "setup-grid", 0, 140, { hide_search: true, default_view: { rows: ["__metrics__"], cols: [], context: ["product", "month"] } }),
    widget("w-plan", "plan-grid", 160, 320, { default_view: { rows: ["product"], cols: ["month"], context: ["__metrics__"] } }),
  ],
};

async function mockDashboard(page: Page) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
}

test("hide_search leaves out the search box of that grid only", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Frozen Dashboard")).toBeVisible({ timeout: 15_000 });
  const cells = page.locator(".mvx-dash-cell");
  await expect(cells.nth(1).getByLabel("Find in this grid")).toBeVisible();
  await expect(cells.nth(0).locator("table")).toBeVisible();
  await expect(cells.nth(0).getByLabel("Find in this grid")).toHaveCount(0);
});

test("the header rows and the row labels stay in view while the grid scrolls", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page);
  await loadAs(page, "dept_head");
  const table = page.locator(".mvx-dash-cell").nth(1).locator("table.mvx-grid-frozen");
  await expect(table.locator("tbody td", { hasText: "Product 30" })).toHaveCount(1, { timeout: 15_000 });

  const read = () => table.evaluate((t: HTMLTableElement) => {
    const box = t.parentElement!.getBoundingClientRect();
    const rows = t.tHead!.rows;
    return {
      year: Math.round(rows[0].cells[1].getBoundingClientRect().top - box.top),
      month: Math.round(rows[1].cells[0].getBoundingClientRect().top - box.top),
      label: Math.round(t.tBodies[0].rows[5].cells[0].getBoundingClientRect().left - box.left),
    };
  });
  const before = await read();
  const scrolled = await table.evaluate((t: HTMLTableElement) => {
    const box = t.parentElement!;
    box.scrollTop = 300;
    box.scrollLeft = 300;
    return { top: box.scrollTop, left: box.scrollLeft };
  });
  // The grid's own box scrolls (both ways) ...
  expect(scrolled.top).toBeGreaterThan(100);
  expect(scrolled.left).toBeGreaterThan(100);
  // ... and the two header rows and the label column stay put.
  await expect.poll(read).toEqual(before);
  expect(before.month).toBeGreaterThan(before.year);
});
