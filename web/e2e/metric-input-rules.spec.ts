/**
 * Input rules (owner's decision 2026-11-09: a small generic rule set on
 * input metrics, refusing writes): a developer gives a number input a lowest
 * and highest accepted value and whole numbers only, and a business user
 * typing a value outside them is told why it was not saved — the server's
 * sentence names the metric, what it accepts, the value and the cell.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

test("the developer sets a metric's accepted values", async ({ page }) => {
  const patches: Record<string, unknown>[] = [];
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) =>
    route.fulfill(json([{ id: "rev-next", name: "Next year", created_at: "2026-09-01T00:00:00Z", is_active: false }])));
  await page.route("**/api/developer/metrics/m1*", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    patches.push(route.request().postDataJSON());
    return route.fulfill(json({ status: "ok" }));
  });
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Metrics", exact: true }).click();
  await page.getByRole("button", { name: "Edit metric revenue" }).click();

  await page.getByLabel("Lowest accepted value").fill("0");
  await page.getByLabel("Highest accepted value").fill("-");
  await page.getByLabel("Highest accepted value").fill("");
  await page.getByLabel("Whole numbers only").check();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => patches.length).toBe(1);
  expect(patches[0].input_rules).toEqual({ min: 0, whole: true });
});

test("a calculated metric has no accepted values to set", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) =>
    route.fulfill(json([{ id: "rev-next", name: "Next year", created_at: "2026-09-01T00:00:00Z", is_active: false }])));
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Metrics", exact: true }).click();
  await page.getByRole("button", { name: "Edit metric gross_margin" }).click();
  await expect(page.getByRole("button", { name: "Save", exact: true })).toBeVisible();
  await expect(page.getByLabel("Lowest accepted value")).toHaveCount(0);
});

const DASH_ID = "rules-dash";
const member = (code: string, label: string) => ({ id: code, code, label });
const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-units", name: "units", label: "Units", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0,
    input_rules: { min: 0, whole: true } }],
  dimensions: [{ id: "product", name: "Product", members: [member("SNACKS", "Snacks")] }],
  departments: [],
  cells: { "m-units:SNACKS": 4 },
  totals: { "m-units": 4 },
  access_rules: { dim_members: {}, metrics: {} },
};
const dashboard = {
  id: DASH_ID, name: "Rules Dashboard", tags: [],
  widgets: [{
    id: "w-grid", widget_type: "grid", ref_id: "rules-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 300,
    widget_props: { default_view: { rows: ["product"], cols: ["__metrics__"], context: [] } },
  }],
};

test("a value the rules refuse is not saved, and the grid says why", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData)));
  await page.route("**/api/cells", (route) => route.fulfill(json(
    { error: "Units accepts values of at least 0, whole numbers only: -5 refused at SNACKS", code: "INPUT_RULE" }, 422)));
  await loadAs(page, "dept_head");

  const input = page.locator("table").first().locator("tbody tr", { has: page.locator("td", { hasText: /^Snacks$/ }) }).locator("input").first();
  await expect(input).toHaveValue("4", { timeout: 15_000 });
  await input.fill("-5");
  await input.press("Enter");
  await expect(page.getByText(/^Save failed: .*Units accepts values of at least 0, whole numbers only: -5 refused at SNACKS$/)).toBeVisible();
});
