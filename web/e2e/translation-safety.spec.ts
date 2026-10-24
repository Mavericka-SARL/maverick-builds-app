/**
 * The console on a page a browser translates. A translator swaps each text
 * node for <font> elements; React, inserting before or removing a text node
 * it no longer finds, failed the page — "Failed to execute 'insertBefore' on
 * 'Node'" (a self-hosting tester, Yandex Browser, 2026-10-23). Text that
 * comes and goes, or follows something that does, is kept in <Txt>.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";
import { findUnsafeText, translatePage } from "./translationSafety";

// A page error, or one a page's error boundary caught (React reports those
// on the console; the boundary keeps the sidebar).
function pageErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => {
    if (m.type() === "error" && /NotFoundError|Failed to execute '(insertBefore|removeChild)'/.test(m.text())) errors.push(m.text());
  });
  return errors;
}

test("no console text a translator breaks: each is kept in <Txt>", () => {
  test.setTimeout(120_000);
  const found = findUnsafeText();
  expect(found.map((f) => `${f.file}:${f.line} (${f.kind}) ${f.source}`),
    "keep this text in <Txt> — `node e2e/translationSafety.ts --fix` does it").toEqual([]);
});

test("translated, a member search's count and a revision switch keep the console", async ({ page }) => {
  const errors = pageErrors(page);
  await mockApi(page);
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Dimensions", exact: true }).click();
  await page.getByRole("button", { name: "Expand Department" }).click();
  const search = page.getByPlaceholder("Search members…");
  await expect(search).toBeVisible({ timeout: 15_000 });

  // "4 results" → "1 result" → "4 results": the plural's text node goes
  // and comes back, each time on a page translated since. (What a
  // translated page shows of a changed number is the translator's: React
  // writes it into the text node the translator took out.)
  await search.fill("a");
  await expect(page.getByText("4 results")).toBeVisible();
  for (const query of ["OPEX", "a", "OPEX"]) {
    await translatePage(page);
    await search.fill(query);
    await expect(search).toBeVisible();
  }

  await page.getByRole("button", { name: "Models", exact: true }).first().click();
  await translatePage(page);
  for (const row of await page.locator(".mvx-admin-revision").all()) await row.click();

  await expect(page.getByRole("navigation", { name: "Primary" })).toBeVisible();
  expect(errors).toEqual([]);
});

const PERIOD = {
  id: "period", name: "Period",
  members: [
    { id: "per-q1", code: "Q1", label: "Q1" },
    { id: "per-jan", code: "JAN", label: "January", parent_code: "Q1" },
    { id: "per-feb", code: "FEB", label: "February", parent_code: "Q1" },
  ],
};

test("translated, a dashboard's selector bar switches its grid and tile", async ({ page }) => {
  const errors = pageErrors(page);
  await mockApi(page);
  const dashboard = {
    id: "tr-dash", name: "Translated Dashboard", tags: [],
    selector_bar: [{ dimension_id: "period", default: "FEB" }],
    widgets: [
      { id: "w-grid", widget_type: "grid", ref_id: "tr-grid", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 500, size_h: 300 },
      { id: "w-kpi", widget_type: "metric_kpi", ref_id: "m-cost", content: null, sort_order: 1, pos_x: 0, pos_y: 320, size_w: 300, size_h: 120, widget_props: { kpi_context_mode: "bar" } },
    ],
  };
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${dashboard.id}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/grid*", async (route) => {
    const scope = JSON.parse(new URL(route.request().url()).searchParams.get("scope") ?? "{}") as Record<string, string>;
    // Slow enough that the grid shows its loading cells between slices.
    await new Promise((resolve) => setTimeout(resolve, 300));
    return route.fulfill(json({
      scenario: "Working", version: "draft", departments: [],
      metrics: [{ id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, dimension_ids: ["period"] }],
      dimensions: [PERIOD],
      cells: { "m-cost:JAN": 100, "m-cost:FEB": 200 },
      totals: { "m-cost": scope.period === "JAN" ? 100 : scope.period === "FEB" ? 200 : 300 },
      access_rules: { dim_members: {}, metrics: {} },
    }));
  });
  await loadAs(page, "dept_head");
  const barPeriod = page.getByRole("toolbar", { name: "Dashboard selectors" }).getByRole("button", { name: "Period", exact: true });
  await expect(barPeriod).toHaveText("February", { timeout: 15_000 });

  for (const month of ["January", "February", "January"]) {
    await translatePage(page);
    await barPeriod.click();
    await page.getByRole("listbox").getByRole("option", { name: month }).click();
    await expect(page.locator(".mvx-kpi__value")).toHaveText(month === "January" ? "100" : "200");
  }
  await expect(page.getByText("Translated Dashboard")).toBeVisible();
  expect(errors).toEqual([]);
});
