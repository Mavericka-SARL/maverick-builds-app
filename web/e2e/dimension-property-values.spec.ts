/**
 * Deleting a dimension property keeps its members' values (the owner's
 * decision, 2026-11-09). Declaring the name again makes formulas read them,
 * so the answer says how many members hold values from before and the
 * property editor offers to clear them; every property also has Clear
 * values, behind a confirmation. Nothing is cleared unless the developer
 * asks:
 *
 *   POST  /api/developer/dimensions/{id}/properties            → {id, existing_values, notice}
 *   PATCH /api/developer/dimensions/{id}/properties/{propId}   {"clear_values": true} → {status, values_cleared}
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

type Patch = { path: string; body: Record<string, unknown> };

async function mockProperties(page: Page) {
  const props = [{ id: "p-seg", dimension_id: "region", name: "segment", data_type: "text" }];
  const members: { id: string; code: string; label: string; properties: Record<string, string> }[] = [
    { id: "m-emea", code: "EMEA", label: "EMEA", properties: { factor: "2" } },
    { id: "m-us", code: "US", label: "US", properties: { factor: "3", segment: "ENT" } },
  ];
  const patches: Patch[] = [];
  const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

  await page.route("**/api/developer/dimensions*", (route) => route.request().method() === "GET"
    ? route.fulfill(json([{ id: "region", name: "Region", agg_rule: "sum", dimension_type: "standard", members }]))
    : route.fallback());
  await page.route("**/api/developer/dimensions/region/properties", (route) => {
    const req = route.request();
    if (req.method() === "GET") return route.fulfill(json(props));
    const body = req.postDataJSON() as { name: string; data_type: string };
    props.push({ id: "p-factor", dimension_id: "region", name: body.name, data_type: body.data_type });
    const held = members.filter((m) => Object.keys(m.properties).some((k) => k.toLowerCase() === body.name.toLowerCase())).length;
    return route.fulfill(json({
      id: "p-factor", existing_values: held,
      ...(held ? { notice: `${held} member(s) already hold values under "${body.name}" from before; formulas read them now — if they are stale, clear them with Clear values on the property (Dimensions › Properties)` } : {}),
    }));
  });
  await page.route("**/api/developer/dimensions/region/properties/*", (route) => {
    const req = route.request();
    if (req.method() !== "PATCH") return route.fallback();
    const path = new URL(req.url()).pathname;
    const body = req.postDataJSON() as Record<string, unknown>;
    patches.push({ path, body });
    const name = props.find((p) => p.id === path.split("/").pop())?.name ?? "";
    let cleared = 0;
    if (body.clear_values) {
      for (const m of members) {
        for (const k of Object.keys(m.properties)) {
          if (k.toLowerCase() === name.toLowerCase()) { delete m.properties[k]; cleared++; }
        }
      }
    }
    return route.fulfill(json({ status: "ok", ...(body.clear_values ? { values_cleared: cleared } : {}) }));
  });
  return patches;
}

async function openProperties(page: Page) {
  await page.getByRole("button", { name: "Dimensions", exact: true }).click();
  await page.getByRole("button", { name: "Expand all", exact: true }).first().click();
  await expect(page.getByPlaceholder("property_name")).toBeVisible({ timeout: 15_000 });
}

async function declare(page: Page, name: string) {
  await page.getByPlaceholder("property_name").fill(name);
  await page.locator(".mvx-admin-inline-form", { has: page.getByPlaceholder("property_name") })
    .getByRole("button", { name: "Add", exact: true }).click();
}

test("declaring a property again says how many members kept values, and clears them on the developer's ask", async ({ page }) => {
  await mockApi(page);
  const patches = await mockProperties(page);
  await loadAs(page, "developer");
  await openProperties(page);

  await declare(page, "factor");
  const offer = page.getByRole("status").filter({ hasText: '2 member(s) already hold values under "factor" from before; formulas read them now' });
  await expect(offer).toBeVisible();
  expect(patches).toEqual([]);

  await offer.getByRole("button", { name: "Clear values", exact: true }).click();
  await expect.poll(() => patches.length).toBe(1);
  expect(patches[0]).toEqual({ path: "/api/developer/dimensions/region/properties/p-factor", body: { clear_values: true } });
  await expect(page.getByRole("status").filter({ hasText: "Cleared factor on 2 members." })).toBeVisible();
  await expect(offer).toBeHidden();
});

test("Keep them leaves the values; a property's Clear values asks first", async ({ page }) => {
  await mockApi(page);
  const patches = await mockProperties(page);
  await loadAs(page, "developer");
  await openProperties(page);

  await declare(page, "factor");
  const offer = page.getByRole("status").filter({ hasText: "already hold values" });
  await expect(offer).toBeVisible();
  await offer.getByRole("button", { name: "Keep them", exact: true }).click();
  await expect(offer).toBeHidden();
  expect(patches).toEqual([]);

  await page.getByRole("button", { name: "Clear values of property segment", exact: true }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toContainText("Clear segment?");
  expect(patches).toEqual([]);
  await dialog.getByRole("button", { name: "Clear values", exact: true }).click();
  await expect.poll(() => patches.length).toBe(1);
  expect(patches[0]).toEqual({ path: "/api/developer/dimensions/region/properties/p-seg", body: { clear_values: true } });
  await expect(page.getByRole("status").filter({ hasText: "Cleared segment on 1 member." })).toBeVisible();
});
