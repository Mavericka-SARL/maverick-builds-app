/**
 * Applications and models can be renamed and described by whoever creates
 * them (the tenant administrator), and text fits its shape (2026-10-13):
 *
 *  - a model or application could only be named once, at creation;
 *  - neither could say what it is for;
 *  - a hyphenated tag ("sales-planning") wrapped inside its fixed-height
 *    pill in the metrics table and spilled out of it;
 *  - a long metric label ran out of its dependency-graph node.
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SHOTS = process.env.E2E_SHOTS_DIR;
const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
const REV = { id: "rev-1", name: "Revision 1", created_at: "2026-10-13T00:00:00Z" };

async function mockModels(page: Page, model: () => { name: string; description: string }, app: () => { name: string; description: string }) {
  await mockApi(page);
  const tenants = () => [{
    id: "t-1", name: "Mavericka", plan: "commercial", created_at: "2026-01-01T00:00:00Z",
    applications: [{
      id: "app-1", ...app(), mode: "planning", status: "active",
      models: [{ id: "model-1", ...model(), storage_type: "oltp", active_revision: REV.name, is_default: true, revisions: [REV] }],
    }],
  }];
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json(tenants())));
  await page.route("**/api/admin/tenants*", (route) => route.fulfill(json(tenants())));
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...REV, is_active: true }])));
}

test("the tenant administrator renames a model and says what it is for", async ({ page }) => {
  const model = { name: "In Order Planning", description: "" };
  const app = { name: "Planning", description: "" };
  let patched: Record<string, unknown> | null = null;
  await mockModels(page, () => model, () => app);
  await page.route("**/api/admin/models/model-1", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    patched = route.request().postDataJSON();
    Object.assign(model, patched);
    return route.fulfill(json({ status: "ok" }));
  });

  await loadAs(page, "dev_admin");
  await expect(page.getByRole("button", { name: "Edit application Planning" })).toBeVisible({ timeout: 15_000 });
  await page.getByRole("button", { name: "Edit model In Order Planning" }).click();
  const editor = page.getByRole("group", { name: "Edit model In Order Planning" });
  await editor.getByLabel("Model name").fill("  Order Planning ");
  await editor.getByLabel("Description").fill("Weekly order proposals per store.");
  if (SHOTS) await page.locator(".mvx-admin-object").first().screenshot({ path: `${SHOTS}/model-editor.png` });
  await editor.getByRole("button", { name: "Save" }).click();

  await expect.poll(() => patched).toEqual({ name: "Order Planning", description: "Weekly order proposals per store." });
  await expect(editor).toHaveCount(0);
  const card = page.locator(".mvx-admin-model", { hasText: "Order Planning" });
  await expect(card).toContainText("Weekly order proposals per store.");
  if (SHOTS) await page.locator(".mvx-admin-object").first().screenshot({ path: `${SHOTS}/models-described.png` });
});

test("a developer who does not administer the tenant cannot rename", async ({ page }) => {
  await mockModels(page, () => ({ name: "In Order Planning", description: "Weekly order proposals." }), () => ({ name: "Planning", description: "" }));
  await loadAs(page, "developer");
  await expect(page.locator(".mvx-admin-model", { hasText: "In Order Planning" })).toContainText("Weekly order proposals.", { timeout: 15_000 });
  await expect(page.getByRole("button", { name: /^Edit (model|application) / })).toHaveCount(0);
});

test("a tag stays inside its pill and a long label inside its graph node", async ({ page }) => {
  await mockApi(page);
  const metrics = [
    { id: "m1", name: "abc_a_threshold", label: "ABC: A class cumulative share threshold", is_input: true, agg_rule: "none", value: null, depends_on: [], depended_by: ["abc_class"], tags: ["sales-planning"] },
    { id: "m2", name: "xyz_x_max_cv", label: "XYZ: X class max coefficient of variation", is_input: true, agg_rule: "none", value: null, depends_on: [], depended_by: ["abc_class"] },
    { id: "m3", name: "abc_class", label: "ABC class", is_input: false, formula: "{abc_a_threshold} + {xyz_x_max_cv}", agg_rule: "none", value: null, depends_on: ["abc_a_threshold", "xyz_x_max_cv"], depended_by: [] },
  ];
  await page.route("**/api/developer/model?*", (route) => route.fulfill(json({ app_name: "Planning", model_name: "Supply", model_id: "model-1", metrics })));
  await page.route("**/api/developer/model", (route) => route.fulfill(json({ app_name: "Planning", model_name: "Supply", model_id: "model-1", metrics })));
  await page.setViewportSize({ width: 1100, height: 800 });
  await loadAs(page, "developer");

  await page.getByRole("button", { name: "Metrics", exact: true }).click();
  const chip = page.locator("tr", { hasText: "abc_a_threshold" }).first().locator(".mvx-filter-chip", { hasText: "sales-planning" });
  await expect(chip).toBeVisible();
  const fit = await chip.evaluate((el) => {
    const label = el.querySelector(".mvx-filter-chip__label") as HTMLElement;
    return { chip: el.getBoundingClientRect().height, label: label.getBoundingClientRect().height };
  });
  expect(fit.chip).toBeLessThanOrEqual(27);
  expect(fit.label).toBeLessThan(20); // one line of 12px text
  if (SHOTS) await page.locator("tr", { hasText: "abc_a_threshold" }).first().screenshot({ path: `${SHOTS}/metric-tag.png` });

  await page.getByRole("button", { name: "Dependency Graph", exact: true }).click();
  const label = page.locator(".mvx-graph-node__label > span", { hasText: "XYZ: X class max coefficient of variation" });
  await expect(label).toBeVisible();
  const node = page.locator("g", { has: label }).locator("rect").first();
  const [l, n] = [await label.boundingBox(), await node.boundingBox()];
  expect(l && n).toBeTruthy();
  expect(l!.x).toBeGreaterThanOrEqual(n!.x);
  expect(l!.x + l!.width).toBeLessThanOrEqual(n!.x + n!.width);
  expect(l!.y + l!.height).toBeLessThanOrEqual(n!.y + n!.height);
  if (SHOTS) await page.locator("g", { has: label }).screenshot({ path: `${SHOTS}/graph-node.png` });
});

test("the tenant administrator describes a model from Applications too", async ({ page }) => {
  const model = { name: "In Order Planning", description: "" };
  let patched: Record<string, unknown> | null = null;
  await mockModels(page, () => model, () => ({ name: "Planning", description: "" }));
  await page.route("**/api/admin/models/model-1", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    patched = route.request().postDataJSON();
    Object.assign(model, patched);
    return route.fulfill(json({ status: "ok" }));
  });

  await loadAs(page, "tenant_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Applications", exact: true }).click();
  await page.getByRole("button", { name: "Edit model In Order Planning" }).click();
  const editor = page.getByRole("group", { name: "Edit model In Order Planning" });
  await editor.getByLabel("Description").fill("Weekly order proposals per store.");
  await editor.getByRole("button", { name: "Save" }).click();

  await expect.poll(() => patched).toEqual({ name: "In Order Planning", description: "Weekly order proposals per store." });
  await expect(page.locator(".mvx-admin-model", { hasText: "In Order Planning" })).toContainText("Weekly order proposals per store.");
});
