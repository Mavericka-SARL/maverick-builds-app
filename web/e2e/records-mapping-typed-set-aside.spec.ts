/**
 * A metric a table's records post into shows its records only: values typed
 * into it before are set aside, kept but read by no grid, chart, export or
 * calculation (owner's decision, 2026-11-09). Creating a mapping, or pointing
 * one at another metric, answers how many (typed_set_aside), and the
 * developer console says so and where they are cleared.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

const mapping = {
  id: "map-1", form_id: "form-1", grid_id: "", name: "Post amounts", source_field: "amount", target_metric_id: "m2",
  aggregation: "sum", posting_statuses: ["approved"], dimension_mappings: {}, live_posting: true, row_filter: [],
};

test("pointing a records mapping at a metric people typed into says what it sets aside", async ({ page }) => {
  await mockApi(page);
  const patched: unknown[] = [];
  await page.route("**/api/developer/form-integrations**", async (route) => {
    const req = route.request();
    if (req.method() === "PATCH") {
      patched.push(req.postDataJSON());
      return route.fulfill(json({ status: "ok", typed_set_aside: 3 }));
    }
    if (req.method() === "GET" && !new URL(req.url()).pathname.endsWith("/preview")) return route.fulfill(json([mapping]));
    return route.fulfill(json({ status: "ok" }));
  });
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.locator(".mvx-source-tile", { hasText: "Post the records of a table" }).click();
  await page.getByRole("button", { name: "Edit" }).click();
  await page.getByLabel("Target input metric").selectOption("m1");
  await page.getByRole("button", { name: "Save", exact: true }).click();

  await expect.poll(() => patched.length).toBe(1);
  await expect(page.getByTestId("typed-set-aside")).toContainText("3 typed values of Revenue are set aside");
  await expect(page.getByTestId("typed-set-aside")).toContainText("Clear values");
});
