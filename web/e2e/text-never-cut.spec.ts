/**
 * Text is never cut off (owner, 2026-11-21): a grid's text cell shows its
 * whole note — typed or read-only — wrapping in place of an ellipsis, and
 * the typed one still saves on Enter. The help desk's status list read
 * "Accepted; someone is working o…".
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const LONG = "Accepted by the service desk; someone is working on it and the requester hears back within the response target of its priority";
const DIM = "status";
const metrics = [
  { id: "m-meaning", name: "st_meaning", label: "What it means", is_input: true, agg_rule: "none", format: "text", format_decimals: 0 },
  { id: "m-locked", name: "st_locked", label: "Fixed wording", is_input: true, readonly: true, agg_rule: "none", format: "text", format_decimals: 0 },
];
const gridData = {
  revision_id: "rev-1", metrics, all_metrics: metrics,
  dimensions: [{ id: DIM, name: "status", display_level: null, members: [
    { id: "s-all", code: "ALL", label: "All statuses" },
    { id: "s-open", code: "OPEN", label: "Open", parent_code: "ALL" },
  ] }],
  departments: [],
  cells: { "m-meaning:OPEN": 0, "m-locked:OPEN": 0 },
  texts: { "m-meaning:OPEN": LONG, "m-locked:OPEN": LONG },
  totals: {}, access_rules: { dim_members: {}, metrics: {} },
};
const dashboard = { id: "d-lists", name: "Lists", tags: [], widgets: [{
  id: "w", widget_type: "grid", ref_id: "g-lists", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 900, size_h: 400,
  widget_props: { default_view: { rows: [DIM], cols: ["__metrics__"], context: [] } } }] };

async function serve(page: Page, writes: unknown[]) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (r) => r.fulfill(json([dashboard])));
  await page.route("**/api/dashboards/d-lists", (r) => r.fulfill(json(dashboard)));
  await page.route("**/api/grid*", (r) => r.fulfill(json(gridData)));
  await page.route("**/api/cells", async (r) => { writes.push(r.request().postDataJSON()); await r.fulfill(json({ status: "ok" })); });
}

test("a grid's text cells show their whole text, typed or read-only", async ({ page }) => {
  const writes: unknown[] = [];
  await mockApi(page);
  await serve(page, writes);
  await loadAs(page, "dept_head");
  const row = page.locator("table").first().getByRole("row", { name: /Open/ });

  const typed = row.getByLabel("What it means — note");
  await expect(typed).toHaveValue(LONG, { timeout: 15_000 });
  // Nothing hidden: the box is as tall as its text and nothing scrolls inside it.
  const box = await typed.evaluate((el: HTMLTextAreaElement) => ({ sh: el.scrollHeight, ch: el.clientHeight, sw: el.scrollWidth, cw: el.clientWidth }));
  expect(box.sh).toBeLessThanOrEqual(box.ch + 1);
  expect(box.sw).toBeLessThanOrEqual(box.cw + 1);

  // The read-only cell wraps its text instead of an ellipsis.
  const fixed = row.locator("td", { hasText: "Accepted by the service desk" }).last();
  const style = await fixed.evaluate((el) => ({ ws: getComputedStyle(el).whiteSpace, to: getComputedStyle(el).textOverflow, sw: el.scrollWidth, cw: el.clientWidth }));
  expect(style.to).not.toBe("ellipsis");
  expect(style.ws).not.toBe("nowrap");
  expect(style.sw).toBeLessThanOrEqual(style.cw + 1);

  // Enter still saves the note.
  await typed.fill("Accepted; someone is working on it");
  await typed.press("Enter");
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).toMatchObject({ metric_id: "m-meaning", text: "Accepted; someone is working on it" });
});

// A bar chart's category labels wrap whole — no "…", a long word split with a
// hyphen — inside the chart, and a value over the tallest bar is not cut at
// the top (the help desk's workload chart read "Unass…" and half a "4").
test("a bar chart's category and value labels are whole", async ({ page }) => {
  const labels = ["Unassigned queue", "Aisha Khan", "Ben Carter", "Chloe Dubois", "Diego Alvarez", "Farah Haddad",
    "Business applications", "Billing and invoices", "Hana Sato"];
  const chartDash = { id: "d-chart", name: "Workload", tags: [], widgets: [{
    id: "w-chart", widget_type: "chart", ref_id: "g-x", content: null, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 360,
    widget_props: { chart: { chart_type: "bar", dimension_id: "agent", metric_ids: ["m-open"], show_values: true } } }] };
  const payload = { chart_type: "bar", as_of: "2026-11-21T00:00:00Z",
    categories: labels.map((label, i) => ({ key: `A${i}`, label })),
    series: [{ metric_id: "m-open", label: "Open tickets", values: [3, 3, 0, 4, 1, 4, 3, 1, 1], format: "number", format_decimals: 0 }],
    context_dims: [], context: {} };
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await mockApi(page);
  await page.route("**/api/dashboards", (r) => r.fulfill(json([chartDash])));
  await page.route("**/api/dashboards/d-chart", (r) => r.fulfill(json(chartDash)));
  await page.route("**/api/dashboard-widgets/w-chart/chart-data", (r) => r.fulfill(json(payload)));
  await loadAs(page, "dept_head");

  const svg = page.locator(".recharts-wrapper svg.recharts-surface").first();
  // Each category label is a group titled with its full name (recharts draws the labels apart from the axis).
  const titles = svg.locator("g > title");
  await expect(titles).toHaveCount(labels.length, { timeout: 15_000 });
  await expect(svg.getByText("…")).toHaveCount(0);
  const ticks = await titles.evaluateAll((ts) => ts.map((t) => ({
    lines: [...t.parentElement!.querySelectorAll("tspan")].map((x) => x.textContent ?? ""),
    bottom: t.parentElement!.getBoundingClientRect().bottom,
  })));
  const box = await svg.boundingBox();
  for (const [i, t] of ticks.entries()) {
    expect(t.lines.join("\n").replace(/-\n/g, "").replace(/\n/g, " ")).toBe(labels[i]);
    expect(t.bottom).toBeLessThanOrEqual(box!.y + box!.height + 1);
  }
  // The values are drawn once the bars' animation ends: on a slow runner,
  // after the category labels (the public CI read none, 2026-11-21).
  const values = svg.locator(".recharts-label-list text");
  await expect(values.first()).toBeVisible({ timeout: 15_000 });
  const tops = await values.evaluateAll((ts) => ts.map((t) => t.getBoundingClientRect().top));
  expect(tops.length).toBeGreaterThan(0);
  for (const top of tops) expect(top).toBeGreaterThanOrEqual(box!.y);
});
