/**
 * A grid's picker lists every metric and dimension in the model. At a hundred
 * metrics that is a wall of checkboxes in which the handful actually in the
 * grid are invisible — so each list is searchable, and split into what is
 * already in the grid and what is not.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

async function openGridConfig(page: Page) {
  await mockApi(page);
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Grids" }).click();
  await page.getByRole("button", { name: "Configure" }).first().click();
}

test("each list separates what is in the grid from what is available", async ({ page }) => {
  await openGridConfig(page);

  // Both sections are labelled and counted, so the current selection is
  // readable without scrolling past everything else in the model.
  await expect(page.getByText(/^In this grid \(\d+\)$/).first()).toBeVisible();
  await expect(page.getByText(/^Available \(\d+\)$/).first()).toBeVisible();
});

test("search filters the metric list", async ({ page }) => {
  await openGridConfig(page);

  const search = page.getByLabel("Search metrics");
  await expect(search).toBeVisible();
  await expect(page.getByRole("checkbox", { name: /Revenue/ })).toBeVisible();

  await search.fill("opex");
  await expect(page.getByRole("checkbox", { name: /Revenue/ })).toHaveCount(0);
  await expect(page.getByRole("checkbox", { name: /OPEX/ }).first()).toBeVisible();

  await search.fill("");
  await expect(page.getByRole("checkbox", { name: /Revenue/ })).toBeVisible();
});

test("search filters the dimension list", async ({ page }) => {
  await openGridConfig(page);

  const search = page.getByLabel("Search dimensions");
  await expect(search).toBeVisible();
  await expect(page.getByRole("checkbox", { name: /Department/ })).toBeVisible();

  await search.fill("region");
  await expect(page.getByRole("checkbox", { name: /Department/ })).toHaveCount(0);
  await expect(page.getByRole("checkbox", { name: /Region/ })).toBeVisible();
});

test("changes wait for Save: Cancel undoes them, Save writes them, Close closes", async ({ page }) => {
  const sent: { method: string; path: string }[] = [];
  await openGridConfig(page);
  await page.route((url) => /^\/api\/developer\/grids\/[^/]+\/(metrics|dimensions)\//.test(url.pathname), (route) => {
    sent.push({ method: route.request().method(), path: new URL(route.request().url()).pathname });
    return route.fulfill({ json: { status: "ok" } });
  });
  // The grid as the server has it: Revenue in it once saved.
  await page.route((url) => url.pathname === "/api/developer/grids", (route) => route.fulfill({ json: [
    { id: "grid-1", name: "OPEX Planning Grid", metric_ids: ["m2", "m3", "m4", ...(sent.length > 0 ? ["m1"] : [])], dimension_ids: ["d1", "d2"] },
  ] }));
  // Revenue is available, not in the grid: ticking it adds it.
  const revenue = page.getByRole("checkbox", { name: /Revenue/ });
  await revenue.check();
  // Nothing is written until Save.
  await expect(page.getByText("Unsaved changes")).toBeVisible();
  expect(sent).toEqual([]);
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByText("Unsaved changes")).toHaveCount(0);
  await expect(revenue).not.toBeChecked();

  await revenue.check();
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => sent.length).toBe(1);
  expect(sent[0]).toEqual({ method: "POST", path: "/api/developer/grids/grid-1/metrics/m1" });

  await page.getByRole("button", { name: "Close" }).click();
  await expect(page.getByLabel("Search metrics")).toHaveCount(0);
});

test("closing with unsaved changes asks first", async ({ page }) => {
  await openGridConfig(page);
  await page.getByRole("checkbox", { name: /Revenue/ }).check();
  await page.getByRole("button", { name: "Close" }).click();
  await expect(page.getByRole("alertdialog")).toContainText("Discard unsaved changes?");
  await page.getByRole("alertdialog").getByRole("button", { name: "Discard changes" }).click();
  await expect(page.getByLabel("Search metrics")).toHaveCount(0);
});
