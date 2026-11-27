/**
 * Two people changing one cell (internal/gateway/cell_conflict.go): a save
 * names the cell as the grid last read it — its number, or blank — and a
 * cell someone changed meanwhile is refused, said so, and shown as it now
 * is, instead of being written over (2026-11-27).
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "conflict-dash";
const member = (code: string, label: string) => ({ id: code, code, label });
const grid = (snacks: number) => ({
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-ly", name: "rev_ly", label: "LY", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 }],
  dimensions: [{ id: "product", name: "Product", members: [member("SNACKS", "Snacks"), member("FRUIT", "Fruit")] }],
  departments: [],
  cells: { "m-ly:SNACKS": snacks },
  totals: { "m-ly": snacks },
  access_rules: { dim_members: {}, metrics: {} },
});
const dashboard = {
  id: DASH_ID, name: "Conflict Dashboard", tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: "conflict-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 300,
    widget_props: { default_view: { rows: ["product"], cols: ["__metrics__"], context: [] } },
  }],
};

type Write = { dim_codes?: Record<string, string>; value?: number; expected?: { value?: number; blank?: boolean } };

async function setup(page: Page, writes: Write[]) {
  const json = (status: number, body: unknown) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
  let snacks = 100;
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json(200, [dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(200, dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(200, grid(snacks))));
  await page.route("**/api/cells", (route) => {
    const body = route.request().postDataJSON() as Write;
    writes.push(body);
    if (body.dim_codes?.product === "SNACKS") {
      // Someone else saved 140 a moment ago.
      snacks = 140;
      return route.fulfill(json(409, {
        error: "someone changed this cell since you opened it: it now holds 140 — look at it again and save once more if you still mean to change it",
        code: "cell_changed", index: 0, metric_id: "m-ly", current: 140,
      }));
    }
    return route.fulfill(json(200, { status: "ok" }));
  });
  await loadAs(page, "dept_head");
}

const cell = (page: Page, row: string) =>
  page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: new RegExp(`^${row}$`) }) }).locator("input").first();

test("a save names the cell as read, and a cell someone changed is refused and shown as it now is", async ({ page }) => {
  const writes: Write[] = [];
  await setup(page, writes);
  const snacks = cell(page, "Snacks");
  await expect(snacks).toHaveValue("100", { timeout: 15_000 });

  await snacks.fill("120");
  await snacks.press("Enter");
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0].expected).toEqual({ value: 100 });
  await expect(page.getByText(/Save failed: (409: )?someone changed this cell since you opened it: it now holds 140/)).toBeVisible();
  await expect(snacks).toHaveValue("140");

  const fruit = cell(page, "Fruit");
  await fruit.fill("7");
  await fruit.press("Enter");
  await expect.poll(() => writes.length).toBe(2);
  expect(writes[1].expected).toEqual({ blank: true });
});
