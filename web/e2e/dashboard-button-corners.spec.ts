/**
 * A button widget's corners (widget_props.button_corners, owner request
 * 2026-10-10): Rounded (default), Sharp or Pill — the same on the dashboard
 * and on the design canvas. Also: a grid's metric selector is labelled
 * "Metric" on the canvas, not its internal id cut to "__metric".
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "corners-dash";
const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
const button = (id: string, corners?: string, x = 0) => ({
  id, widget_type: "automation_button", ref_id: "rule-1", content: `Run ${corners ?? "default"}`, sort_order: 0,
  pos_x: x, pos_y: 0, size_w: 200, size_h: 60, widget_props: corners ? { button_corners: corners } : {},
});
const dashboard = {
  id: DASH_ID, name: "Corners Dashboard", tags: [],
  widgets: [
    button("w-sharp", "sharp", 0), button("w-pill", "pill", 220), button("w-default", undefined, 440),
    {
      id: "w-grid", widget_type: "grid", ref_id: "grid-1", content: null, sort_order: 1, pos_x: 0, pos_y: 100, size_w: 600, size_h: 200,
      widget_props: { default_view: { rows: ["region"], cols: [], context: ["__metrics__"] } },
    },
  ],
};
const rule = { id: "rule-1", application_id: "app-1", name: "Run", trigger_type: "manual", enabled: true, workflow_name: "WF", created_at: "2026-10-01T00:00:00Z" };

async function mockCorners(page: Page) {
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  await page.route("**/api/automation/rules*", (route) => route.fulfill(json([rule])));
  await page.route("**/api/developer/dashboards*", (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    return route.fulfill(json([dashboard]));
  });
  await page.route("**/api/developer/grids*", (route) => route.fulfill(json([
    { id: "grid-1", name: "Budget", revision_id: "rev-1", metric_ids: ["m1"], dimension_ids: ["region"] },
  ])));
}

const radiusOf = (page: Page, label: string, selector: string) =>
  page.locator(selector, { hasText: label }).first().evaluate(el => getComputedStyle(el).borderTopLeftRadius);

test("a button's corners on the dashboard follow its setting", async ({ page }) => {
  await mockCorners(page);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Corners Dashboard")).toBeVisible({ timeout: 15_000 });
  await expect(page.locator(".mvx-command-button", { hasText: "Run sharp" })).toBeVisible();
  expect(await radiusOf(page, "Run sharp", ".mvx-command-button")).toBe("0px");
  expect(await radiusOf(page, "Run pill", ".mvx-command-button")).toBe("999px");
  expect(await radiusOf(page, "Run default", ".mvx-command-button")).not.toBe("0px");
});

test("the design canvas draws the corners and sets them; the metric selector reads Metric", async ({ page }) => {
  await mockCorners(page);
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Dashboards", exact: true }).first().click();
  await page.getByRole("button", { name: "Design", exact: true }).first().click();
  await expect(page.locator(".mvx-canvas-widget").first()).toBeVisible({ timeout: 15_000 });

  expect(await radiusOf(page, "Run sharp", ".mvx-canvas-widget button")).toBe("0px");
  expect(await radiusOf(page, "Run pill", ".mvx-canvas-widget button")).toBe("999px");

  const selectors = page.getByLabel("Widget selectors");
  await expect(selectors).toContainText("Metric");
  await expect(selectors).not.toContainText("__metric");

  // Choosing Sharp for the default button squares it on the canvas at once.
  await page.locator(".mvx-canvas-widget", { hasText: "Run default" }).first().click();
  await page.getByLabel("Button corners").selectOption("sharp");
  expect(await radiusOf(page, "Run default", ".mvx-canvas-widget button")).toBe("0px");
});
