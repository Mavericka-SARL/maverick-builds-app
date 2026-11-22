/**
 * An unchanged poll comes back as a 304 and the tile keeps its value.
 *
 * The gateway tags a grid read with an ETag (read_etag.go); the console
 * keeps the answer with its tag and sends the tag back on the next poll
 * (api.getGrid → apiFetchRevalidated). A 304 means nothing the answer
 * depends on changed: the kept answer stands, without the server working it
 * out again. When the data changes, the tag does, and the poll gets the new
 * answer.
 *
 * Self-contained route mocks, like kpi-alignment.spec.ts.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "revalidate-dash";

const metrics = [
  { id: "m-cost", name: "cost", label: "Cost", is_input: true, agg_rule: "sum", format: "currency", format_currency: "$", format_decimals: 0, dimension_ids: [] },
];

const meta = {
  scenario: "Working",
  version: "draft",
  metrics,
  dimensions: [],
  departments: [],
  cells: {},
  totals: {},
  access_rules: { dim_members: {}, metrics: {} },
};

const dashboard = {
  id: DASH_ID,
  name: "Revalidation Dashboard",
  tags: [],
  widgets: [
    {
      id: "w-cost", widget_type: "metric_kpi", ref_id: "m-cost", content: null,
      sort_order: 0, pos_x: 0, pos_y: 0, size_w: 300, size_h: 140, widget_props: { kpi_context_mode: "total" },
    },
  ],
};

test("an unchanged poll is answered 304 and the tile keeps its value; a change comes through", async ({ page }) => {
  let generation = 1;
  const costAt: Record<number, number> = { 1: 90000, 2: 95000 };
  let conditional = 0;
  let notModified = 0;

  await mockApi(page);
  await page.route("**/api/dashboards", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([dashboard]) })
  );
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(dashboard) })
  );
  await page.route("**/api/grid*", (route) => {
    const url = new URL(route.request().url());
    if (url.searchParams.get("totals_only") !== "1") {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(meta) });
    }
    const tag = `W/"g${generation}"`;
    const sent = route.request().headers()["if-none-match"];
    if (sent) conditional++;
    if (sent === tag) {
      notModified++;
      return route.fulfill({ status: 304, headers: { ETag: tag } });
    }
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      headers: { ETag: tag, "Cache-Control": "private, no-store" },
      body: JSON.stringify({ ...meta, metrics: [], totals: { "m-cost": costAt[generation] } }),
    });
  });
  await loadAs(page, "dept_head");

  await expect(page.getByText("Revalidation Dashboard")).toBeVisible({ timeout: 15_000 });
  const value = page.locator(".mvx-kpi__value");
  await expect(value).toHaveText("$90,000");

  // The tile polls every two seconds: its polls send the tag back and are
  // answered 304, and the value stays.
  await expect.poll(() => notModified, { timeout: 10_000 }).toBeGreaterThanOrEqual(2);
  expect(conditional).toBeGreaterThanOrEqual(notModified);
  await expect(value).toHaveText("$90,000");

  // The data changes: the tag does, and the next poll brings the new value.
  generation = 2;
  await expect(value).toHaveText("$95,000", { timeout: 10_000 });
  const before = notModified;
  await expect.poll(() => notModified, { timeout: 10_000 }).toBeGreaterThan(before);
  await expect(value).toHaveText("$95,000");
});
