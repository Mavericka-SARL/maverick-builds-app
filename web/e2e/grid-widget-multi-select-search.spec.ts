/**
 * Grid widgets (owner request, 2026-10-10):
 *  - widget_props.multi_select lets a context selector take several members
 *    (Region 1 + Region 3): the grid shows them added up by each metric's own
 *    rule, read-only, and a formula metric — which only the server can
 *    aggregate — shows "—" rather than an invented number;
 *  - "Find a row or column" narrows the axes to what matches.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "multi-dash";
const GRID_ID = "multi-grid";

const member = (code: string, label: string, parent_code?: string) => ({ id: code, code, label, ...(parent_code ? { parent_code } : {}) });
const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [
    { id: "m-costs", name: "costs", label: "Costs", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 },
    { id: "m-margin", name: "margin", label: "Margin", is_input: false, agg_rule: "formula", format: "number", format_decimals: 0 },
  ],
  dimensions: [
    { id: "product", name: "Product", members: [
      member("LIP", "Lipitor"), member("LIP10", "Lipitor 10 mg", "LIP"), member("LIP20", "Lipitor 20 mg", "LIP"),
      member("CRE", "Crestor"), member("CRE5", "Crestor 5 mg", "CRE"),
    ] },
    { id: "region", name: "Region", members: [
      member("ALL", "All Regions"), member("R1", "Region 1", "ALL"), member("R2", "Region 2", "ALL"), member("R3", "Region 3", "ALL"),
    ] },
  ],
  departments: [],
  cells: {
    "m-costs:LIP10:R1": 10, "m-costs:LIP10:R2": 20, "m-costs:LIP10:R3": 30,
    "m-costs:LIP20:R1": 1, "m-costs:LIP20:R3": 3, "m-costs:CRE5:R2": 100,
    "m-margin:LIP10:R1": 7,
  },
  totals: { "m-costs": 164, "m-margin": 7 },
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = (multi: boolean) => ({
  id: DASH_ID,
  name: "Multi Dashboard",
  tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 800, size_h: 320,
    widget_props: {
      default_view: { rows: ["product"], cols: ["__metrics__"], context: ["region"], filter_sel: { region: "R1" } },
      ...(multi ? { multi_select: true } : {}),
    },
  }],
});

async function mockDashboard(page: Page, multi: boolean, scopes: string[]) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard(multi)])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard(multi))));
  await page.route("**/api/grid*", (route) => {
    scopes.push(new URL(route.request().url()).searchParams.get("scope") ?? "");
    return route.fulfill(json(gridData));
  });
}

const row = (page: Page, label: string) =>
  page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: new RegExp(`^└?${label}$`) }) });

test("a multi-select context adds up the chosen members, read-only", async ({ page }) => {
  const scopes: string[] = [];
  await mockApi(page);
  await mockDashboard(page, true, scopes);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Multi Dashboard")).toBeVisible({ timeout: 15_000 });

  // One region: the typed leaf, editable.
  await expect(row(page, "Lipitor 10 mg").locator("input").first()).toHaveValue("10");

  const trigger = page.getByRole("button", { name: "Region context" });
  await trigger.click();
  await page.getByRole("option", { name: /Region 3/ }).click();
  // Still open: a second choice toggles without closing.
  await expect(page.getByRole("option", { name: /Region 1/ })).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Escape");
  await expect(trigger).toHaveText("Region 1 + Region 3");

  await expect(row(page, "Lipitor 10 mg").locator("td").nth(1)).toHaveText("40");
  await expect(row(page, "Lipitor 20 mg").locator("td").nth(1)).toHaveText("4");
  await expect(row(page, "Lipitor").first().locator("td").nth(1)).toHaveText("44");
  await expect(page.locator("table").first().locator("input")).toHaveCount(0);
  // A formula metric has no value for an ad-hoc set of regions.
  await expect(row(page, "Lipitor 10 mg").locator("td").nth(2)).toHaveText("—");
  // The whole Region dimension is read, not one pinned member.
  await expect.poll(() => scopes[scopes.length - 1]).not.toContain("region");

  // Back to one region: the leaf is editable again.
  await trigger.click();
  await page.getByRole("option", { name: /Region 1/ }).click();
  await page.keyboard.press("Escape");
  await expect(trigger).toHaveText("Region 3");
  await expect(row(page, "Lipitor 10 mg").locator("input").first()).toHaveValue("30");
});

test("without multi_select a selector takes one member", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page, false, []);
  await loadAs(page, "dept_head");
  const trigger = page.getByRole("button", { name: "Region context" });
  await expect(trigger).toHaveText("Region 1", { timeout: 15_000 });
  await trigger.click();
  await page.getByRole("option", { name: /Region 3/ }).click();
  await expect(trigger).toHaveText("Region 3");
  await expect(page.locator(".mvx-hier-select__popover")).toHaveCount(0); // closed on the first choice
});

test("Find narrows the rows and the metric columns to what matches", async ({ page }) => {
  await mockApi(page);
  await mockDashboard(page, true, []);
  await loadAs(page, "dept_head");
  const find = page.getByLabel("Find in this grid");
  await expect(find).toBeVisible({ timeout: 15_000 });

  await find.fill("20 mg");
  await expect(row(page, "Lipitor 20 mg")).toHaveCount(1);
  await expect(row(page, "Lipitor")).toHaveCount(1); // its parent, for context
  await expect(row(page, "Lipitor 10 mg")).toHaveCount(0);
  await expect(row(page, "Crestor")).toHaveCount(0);

  await find.fill("crestor");
  await expect(row(page, "Crestor 5 mg")).toHaveCount(1); // a matching parent shows what it adds up

  await find.fill("margin");
  const headers = (await page.locator("table").first().locator("thead th").allTextContents()).map(t => t.trim());
  expect(headers.join(" ")).toContain("Margin");
  expect(headers.join(" ")).not.toContain("Costs");

  await find.fill("zzz");
  await expect(page.getByText("No match")).toBeVisible();
  await expect(row(page, "Crestor 5 mg")).toHaveCount(1);
});
