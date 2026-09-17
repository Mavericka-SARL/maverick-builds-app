/**
 * Regression gates for the responsive shell: the collapsible sidebar rail and
 * KPI tile container sizing (values must never wrap mid-number) at two viewports.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

async function setPersona(page: Page, persona: string) {
  await page.addInitScript((p) => {
    if (!localStorage.getItem("dev_persona")) {
      localStorage.setItem("dev_persona", p);
    }
  }, persona);
}

test("sidebar collapses to icon rail and persists", async ({ page }) => {
  await setPersona(page, "dept_head");
  await page.goto("/");
  await expect(page.getByRole("button", { name: "Dashboards" })).toBeVisible({ timeout: 10_000 });
  await page.screenshot({ path: "test-results/tmp-sidebar-expanded.png" });

  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  // Label text hidden (the button itself stays, icon-only, named via title).
  await expect(page.getByRole("button", { name: "Expand sidebar" })).toBeVisible();
  await expect(page.locator(".mvx-sidebar-nav__label", { hasText: "Dashboards" })).not.toBeVisible();
  const railWidth = await page.locator(".mvx-app-shell__sidebar").evaluate((el) => el.getBoundingClientRect().width);
  expect(railWidth).toBeLessThan(80);
  await page.screenshot({ path: "test-results/tmp-sidebar-collapsed.png" });

  // Survives reload.
  await page.reload();
  await expect(page.getByRole("button", { name: "Expand sidebar" })).toBeVisible({ timeout: 10_000 });
  const railWidth2 = await page.locator(".mvx-app-shell__sidebar").evaluate((el) => el.getBoundingClientRect().width);
  expect(railWidth2).toBeLessThan(80);

  // Expand restores labels.
  await page.getByRole("button", { name: "Expand sidebar" }).click();
  await expect(page.getByRole("button", { name: "Dashboards" })).toBeVisible();
});

// Four KPI tiles abreast, one holding "1,300,000" — the exact shape that used
// to wrap mid-number ("100,00 / 0" reported live).
const KPI_DASH = {
  id: "dash-1",
  name: "KPI stress",
  tags: ["finance"],
  folder_id: null,
  widgets: ["m2", "m3", "m4", "m6"].map((mid, i) => ({
    id: `kw${i}`,
    widget_type: "metric_kpi",
    ref_id: mid,
    content: null,
    sort_order: i,
    pos_x: i * 240,
    pos_y: 0,
    size_w: 240,
    size_h: 120,
  })),
};

for (const width of [1280, 760]) {
  test(`KPI value never wraps mid-number at ${width}px`, async ({ page }) => {
    await mockApi(page);
    await page.route("**/api/dashboards", (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([KPI_DASH]) }));
    await page.route("**/api/dashboards/dash-1", (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(KPI_DASH) }));
    await page.setViewportSize({ width, height: 900 });
    await loadAs(page, "dept_head");

    const kpis = page.locator(".mvx-kpi__value");
    await expect(kpis.first()).toBeVisible({ timeout: 10_000 });
    const n = await kpis.count();
    expect(n).toBeGreaterThanOrEqual(4);
    for (let i = 0; i < n; i++) {
      const box = await kpis.nth(i).boundingBox();
      if (!box) continue;
      const lineHeight = await kpis.nth(i).evaluate((el) => parseFloat(getComputedStyle(el).fontSize) * 1.1);
      // One line only, and no horizontal spill out of the tile.
      expect(box.height).toBeLessThan(lineHeight * 1.6);
      const cell = await kpis.nth(i).evaluate((el) => (el.closest(".mvx-dash-cell") as HTMLElement).getBoundingClientRect().width);
      expect(box.width).toBeLessThanOrEqual(cell + 1);
    }
    await page.screenshot({ path: `test-results/tmp-kpi-${width}.png` });
  });
}
