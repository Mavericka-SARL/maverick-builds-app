/**
 * A selector that keeps its choice (selector_bar entry keep_choice, owner
 * 2026-11-27): the member a person picks opens the next dashboard whose
 * selector on the same dimension keeps its choice too — a project picked on
 * Project · Overview opens Project · Costs on it. A dashboard without the
 * setting opens on its default and leaves the kept choice alone; a kept
 * member a dashboard does not offer gives way to its default there, and that
 * default is not kept.
 *
 * Same self-contained route-mock style as dashboard-selector-bar.spec.ts.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const dimensions = [
  {
    id: "period", name: "Period",
    members: [
      { id: "per-q1", code: "Q1", label: "Q1" },
      { id: "per-jan", code: "JAN", label: "January", parent_code: "Q1" },
      { id: "per-feb", code: "FEB", label: "February", parent_code: "Q1" },
      { id: "per-mar", code: "MAR", label: "March", parent_code: "Q1" },
    ],
  },
];

const gridData = {
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, dimension_ids: ["period"] }],
  dimensions,
  departments: [],
  cells: {},
  totals: { "m-cost": 0 },
  access_rules: { dim_members: {}, metrics: {} },
};

const tile = (id: string) => ({ id, widget_type: "metric_kpi", ref_id: "m-cost", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 300, size_h: 120, widget_props: { kpi_context_mode: "bar" } });
const dashboards = [
  { id: "d-one", name: "Project · Overview", tags: [], selector_bar: [{ dimension_id: "period", default: "FEB", keep_choice: true }], widgets: [tile("w1")] },
  { id: "d-two", name: "Project · Costs", tags: [], selector_bar: [{ dimension_id: "period", default: "FEB", keep_choice: true }], widgets: [tile("w2")] },
  { id: "d-three", name: "Portfolio", tags: [], selector_bar: [{ dimension_id: "period", default: "FEB" }], widgets: [tile("w3")] },
  { id: "d-four", name: "March only", tags: [], selector_bar: [{ dimension_id: "period", default: "MAR", members: ["MAR"], keep_choice: true }], widgets: [tile("w4")] },
];

async function mockDashboards(page: Page) {
  await page.route("**/api/dashboards", (route) => route.fulfill({ json: dashboards }));
  for (const d of dashboards) await page.route(`**/api/dashboards/${d.id}`, (route) => route.fulfill({ json: d }));
  await page.route("**/api/grid*", (route) => route.fulfill({ json: gridData }));
}

test("a selector that keeps its choice opens the next such dashboard on the member picked", async ({ page }) => {
  await mockApi(page);
  await mockDashboards(page);
  await loadAs(page, "dept_head");

  const period = page.getByRole("toolbar", { name: "Dashboard selectors" }).getByRole("button", { name: "Period", exact: true });
  const open = async (name: string) => {
    await page.getByRole("button", { name, exact: true }).first().click();
    await expect(page.getByRole("button", { name, exact: true }).first()).toHaveAttribute("aria-pressed", "true");
  };

  // Opening a dashboard is not a choice: the next one opens on its own default.
  await open("Project · Overview");
  await expect(period).toHaveText("February", { timeout: 15_000 });
  await period.click();
  await page.getByRole("listbox").getByRole("option", { name: "January" }).click();
  await expect(period).toHaveText("January");

  await open("Project · Costs");
  await expect(period).toHaveText("January");

  // Without the setting: its default, and the kept choice stays as it was.
  await open("Portfolio");
  await expect(period).toHaveText("February");
  await period.click();
  await page.getByRole("listbox").getByRole("option", { name: "March" }).click();
  await expect(period).toHaveText("March");
  await open("Project · Costs");
  await expect(period).toHaveText("January");

  // Not offered here: the default, which is not kept either.
  await open("March only");
  await expect(period).toHaveText("March");
  await open("Project · Overview");
  await expect(period).toHaveText("January");
});
