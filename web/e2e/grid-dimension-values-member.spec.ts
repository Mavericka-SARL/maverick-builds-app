/**
 * A grid gaining a dimension while its metrics hold values asks which member
 * of it they belong to (409 VALUES_NEED_MEMBER) and saves again with the
 * answer; the values were left keyed without the dimension, where no cell
 * and no formula read them.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("adding a dimension asks which member the stored values belong to", async ({ page }) => {
  const sent: { path: string; body: unknown }[] = [];
  await mockApi(page);
  await page.route((url) => url.pathname === "/api/developer/grids", (route) => route.fulfill({ json: [
    { id: "grid-1", name: "OPEX Planning Grid", metric_ids: ["m2", "m3"], dimension_ids: ["d1", ...(sent.length > 1 ? ["d2"] : [])] },
  ] }));
  await page.route((url) => /^\/api\/developer\/grids\/[^/]+\/dimensions\//.test(url.pathname), (route) => {
    const raw = route.request().postData();
    sent.push({ path: new URL(route.request().url()).pathname, body: raw ? JSON.parse(raw) : null });
    if (sent.length === 1) {
      return route.fulfill({ status: 409, json: { code: "VALUES_NEED_MEMBER", error: "the grid's metrics hold 3 values without Region", dimension_ids: ["d2"], count: 3 } });
    }
    return route.fulfill({ json: { status: "ok", moved: 3 } });
  });
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Grids" }).click();
  await page.getByRole("button", { name: "Configure" }).first().click();

  await page.getByRole("checkbox", { name: /Region/ }).check();
  await page.getByRole("button", { name: "Save" }).click();

  const dialog = page.getByRole("dialog", { name: "Which member do the values belong to?" });
  await expect(dialog).toContainText("3 values are already stored without Region");
  // Only members that take input are offered: Global has members under it.
  const select = dialog.getByLabel("Member of Region");
  await expect(select.locator("option")).toHaveText(["EMEA", "North America"]);
  await select.selectOption("NA");
  await dialog.getByRole("button", { name: "File the values there" }).click();

  await expect.poll(() => sent.length).toBe(2);
  expect(sent[0]).toEqual({ path: "/api/developer/grids/grid-1/dimensions/d2", body: null });
  expect(sent[1]).toEqual({ path: "/api/developer/grids/grid-1/dimensions/d2", body: { member: "NA" } });
  await expect(page.getByText("3 stored values filed under the member chosen for Region.")).toBeVisible();
});
