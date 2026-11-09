/**
 * A model import says what it left out of the package (2026-11-09): a link
 * to a row the package does not hold (a typo in a hand-edited package), a
 * member's parent in an unrelated dimension, a widget setting the console
 * does not read. The server named them in the answer's notes; the console
 * showed nothing, so the tenant administrator never learned of them.
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
const REV = { id: "rev-1", name: "Revision 1", created_at: "2026-11-09T00:00:00Z" };

test("the tenant administrator sees what an import left out", async ({ page }) => {
  await mockApi(page);
  const tenants = () => [{
    id: "t-1", name: "Mavericka", plan: "commercial", created_at: "2026-01-01T00:00:00Z",
    applications: [{
      id: "app-1", name: "Planning", description: "", mode: "planning", status: "active",
      models: [{ id: "model-1", name: "Sales", description: "", storage_type: "oltp", active_revision: REV.name, is_default: true, revisions: [REV] }],
    }],
  }];
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json(tenants())));
  await page.route("**/api/admin/tenants*", (route) => route.fulfill(json(tenants())));
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...REV, is_active: true }])));
  const notes = [
    `grid "Plan": metric "met-revv" is not in the package, so the grid leaves it out`,
    `dimension "Product", member "P1": its parent "ALL" is a member of "Region", which is neither "Product" nor its parent dimension, so the member is imported without a parent`,
  ];
  let posted: Record<string, unknown> | null = null;
  await page.route("**/api/admin/models/import", (route) => {
    posted = route.request().postDataJSON();
    return route.fulfill(json({ model_id: "model-2", revision_id: "rev-2", notes }));
  });

  await loadAs(page, "dev_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Applications" }).click();
  await expect(page.getByRole("button", { name: "Import model" })).toBeVisible({ timeout: 15_000 });
  await page.locator('input[type="file"][accept="application/json,.json"]').setInputFiles({
    name: "hand-made.mavericks-model.json", mimeType: "application/json",
    buffer: Buffer.from(JSON.stringify({ format: "mavericks-model-export", version: 2, model_name: "Hand-made" })),
  });

  await expect.poll(() => posted?.application_id).toBe("app-1");
  const leftOut = page.getByRole("status", { name: "What the import left out" });
  await expect(leftOut).toContainText("Imported. Left out of the package:");
  for (const n of notes) await expect(leftOut).toContainText(n);
  await leftOut.getByRole("button", { name: "Dismiss" }).click();
  await expect(leftOut).toHaveCount(0);
});
