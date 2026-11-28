/**
 * The Dashboards page's tabs on one line that scrolls sideways (owner,
 * 2026-11-27: thirty dashboards wrapped to six lines of tabs): every tab on
 * the same line, an arrow at an edge that hides tabs, and the open
 * dashboard's tab kept in view.
 *
 * Self-contained route mocks, as dashboard-selector-bar.spec.ts.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const dashboards = Array.from({ length: 30 }, (_, i) => ({
  id: `d-${i}`, name: `Area ${Math.floor(i / 5) + 1} · Dashboard number ${i + 1}`, tags: [], widgets: [
    { id: `t-${i}`, widget_type: "text", ref_id: null, content: `Dashboard ${i + 1}`, sort_order: 0, pos_x: 0, pos_y: 0, size_w: 600, size_h: 60 },
  ],
}));

test("thirty dashboards: one line of tabs, arrows at the hidden edge, the open tab in view", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/dashboards", (route) => route.fulfill({ json: dashboards }));
  for (const d of dashboards) await page.route(`**/api/dashboards/${d.id}`, (route) => route.fulfill({ json: d }));
  await loadAs(page, "dept_head");

  const first = page.getByRole("button", { name: dashboards[0].name, exact: true });
  await expect(first).toBeVisible({ timeout: 15_000 });
  const tops = await page.locator(".mvx-tabs--scroll .mvx-tabs__button").evaluateAll(bs => [...new Set(bs.map(b => Math.round(b.getBoundingClientRect().top)))]);
  expect(tops).toHaveLength(1);

  await expect(page.getByRole("button", { name: "Show earlier tabs" })).toHaveCount(0);
  await page.getByRole("button", { name: "Show more tabs" }).click();
  await expect(page.getByRole("button", { name: "Show earlier tabs" })).toBeVisible();

  // The last dashboard opened (as from a link or a search): its tab is in view.
  await page.getByRole("button", { name: dashboards[29].name, exact: true }).click();
  await expect(page.getByText("Dashboard 30", { exact: true })).toBeVisible();
  const inView = await page.locator(".mvx-tabs--scroll").evaluate(row => {
    const b = row.querySelector<HTMLElement>('[aria-pressed="true"]')!;
    const r = row.getBoundingClientRect(), t = b.getBoundingClientRect();
    return t.left >= r.left - 1 && t.right <= r.right + 1;
  });
  expect(inView).toBe(true);
});
