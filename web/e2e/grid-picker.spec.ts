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

test("the panel closes with Done, since every change is already saved", async ({ page }) => {
  await openGridConfig(page);
  // "Close" next to a delete button reads as though there might be something
  // pending to discard; there never is.
  await expect(page.getByRole("button", { name: "Done" })).toBeVisible();
  await page.getByRole("button", { name: "Done" }).click();
  await expect(page.getByLabel("Search metrics")).toHaveCount(0);
});
