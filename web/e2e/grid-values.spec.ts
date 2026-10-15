/**
 * Grids › Values: a developer fills a grid with sample values, or clears
 * its values, across the grid or under members — counted first. The only
 * bulk clear was a full reload of the whole revision (a tester, 2026-10-15).
 *
 * The server side is internal/gateway/values_clear_test.go and
 * values_sample_test.go.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

test("a developer fills a grid with sample values, then clears part of it", async ({ page }) => {
  await mockApi(page);
  const sample: Array<Record<string, unknown>> = [];
  const clear: Array<Record<string, unknown>> = [];
  await page.route("**/api/developer/grids/*/sample-values", async (route) => {
    const body = route.request().postDataJSON();
    sample.push(body);
    return route.fulfill(json({ cells: 24, withheld: 0, dry_run: !!body.dry_run }));
  });
  await page.route("**/api/developer/grids/*/clear-values", async (route) => {
    const body = route.request().postDataJSON();
    clear.push(body);
    return route.fulfill(json({ cells: 6, withheld: 1, posted: 0, dry_run: !!body.dry_run }));
  });

  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Grids" }).click();
  await page.getByRole("button", { name: "Values of OPEX Planning Grid" }).click();
  const dialog = page.getByRole("dialog", { name: "Values of OPEX Planning Grid" });

  await dialog.getByRole("button", { name: "Preview" }).click();
  await expect(dialog.getByRole("status")).toContainText("24 cells would be filled");
  expect(sample[0].dry_run).toBe(true);
  await dialog.getByRole("button", { name: "Fill", exact: true }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Fill" }).click();
  await expect(dialog.getByText("Filled 24 cells.")).toBeVisible();
  expect(sample[1].dry_run).toBe(false);
  expect(Object.keys(sample[1].metrics as object).length).toBeGreaterThan(0);

  // The tabs are pressed buttons; "Clear values" is also the action's name.
  await dialog.locator(".mvx-tabs").getByRole("button", { name: "Clear values" }).click();
  await dialog.getByLabel("Region members").fill("NA, EMEA");
  await dialog.getByRole("button", { name: "Count" }).click();
  await expect(dialog.getByRole("status")).toContainText("6 cells would be cleared; 1 left that you may not write");
  await dialog.getByRole("button", { name: "Clear values" }).last().click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Clear values" }).click();
  await expect(dialog.getByText("Cleared 6 cells.")).toBeVisible();
  expect(clear.at(-1)?.dry_run).toBe(false);
  expect(Object.values(clear.at(-1)?.members as Record<string, string[]>)).toEqual([["NA", "EMEA"]]);
});
