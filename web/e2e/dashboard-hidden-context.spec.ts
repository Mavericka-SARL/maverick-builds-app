/**
 * A chart whose saved context_defaults pin a member that is HIDDEN from the
 * viewer must still render: the server resolves for the dimension's default
 * visible member and reports it in `context`; the widget adopts that value
 * so its selector shows the member the chart really shows, and the next
 * request carries the visible member (no 403 retry loop — reported live
 * 2026-09-13 for a business user with "Laptop" hidden and a line chart
 * pinned to Laptop: "Unable to load chart data. Retry." forever).
 *
 * Self-contained: CI runs without a gateway. The mock plays the server's
 * rule: a request for the hidden member answers with the substituted one.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test.use({ actionTimeout: 10_000 });

const DASH_ID = "hidden-ctx-dash";
const PERIOD = "period";
const REGION = "region";

const dashboard = {
  id: DASH_ID,
  name: "Hidden Context Dashboard",
  tags: [],
  widgets: [
    {
      id: "w-chart", widget_type: "chart", ref_id: "grid-x", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 360,
      widget_props: { chart: { chart_type: "bar", dimension_id: REGION, metric_ids: ["m-units"], context_defaults: { [PERIOD]: "FEB" } } },
    },
    // A bare widget type given the "white card" surface by the developer.
    { id: "w-text", widget_type: "text", ref_id: null, content: "CARDED-TEXT", sort_order: 1, pos_x: 600, pos_y: 0, size_w: 300, size_h: 60, widget_props: { background: "white" } },
  ],
};

function chartPayload(period: string) {
  return {
    chart_type: "bar",
    as_of: "2026-09-13T00:00:00Z",
    categories: [{ key: "EU", label: "Europe" }, { key: "US", label: "United States" }],
    series: [{ metric_id: "m-units", label: "Units", values: period === "JAN" ? [10, 20] : [1, 2], format: "number", format_decimals: 0, format_currency: "" }],
    // FEB is hidden from this viewer, so it is not among the selectable members.
    context_dims: [{ id: PERIOD, name: "Period", members: [{ code: "JAN", label: "January" }, { code: "MAR", label: "March" }] }],
    context: { [PERIOD]: period },
  };
}

test("a chart pinned to a hidden member resolves for a visible one and says so", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dashboard])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dashboard)));
  const requested: string[] = [];
  await page.route("**/api/dashboard-widgets/w-chart/chart-data", (route) => {
    const body = route.request().postDataJSON() as { context?: Record<string, string> };
    const asked = body?.context?.[PERIOD] ?? "";
    requested.push(asked);
    // The server's rule: a hidden member is swapped for the default visible one.
    return route.fulfill(json(chartPayload(asked === "FEB" ? "JAN" : asked || "JAN")));
  });

  await loadAs(page, "dept_head");
  await expect(page.getByText("Hidden Context Dashboard")).toBeVisible({ timeout: 15_000 });

  // The widget renders (no error state) and its selector shows the member
  // the data is really for.
  await expect(page.getByLabel("Period")).toHaveText("January", { timeout: 15_000 });
  await expect(page.getByText("Unable to load chart data")).toHaveCount(0);
  // First request carried the saved default; every later one the visible member.
  expect(requested[0]).toBe("FEB");
  await expect.poll(() => requested.length, { timeout: 10_000 }).toBeGreaterThan(1);
  expect(requested.slice(1).every((p) => p === "JAN")).toBe(true);

  // Developer-chosen surface: the text widget sits on a card.
  const card = page.locator(".mvx-dash-cell--card");
  await expect(card).toHaveCount(1);
  await expect(card).toContainText("CARDED-TEXT");
});
