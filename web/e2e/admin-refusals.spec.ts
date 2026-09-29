/**
 * The server refuses some access changes a tenant admin may otherwise make:
 * removing the last application or model grant that narrows a developer with
 * no tenant, directly or by deleting the model (removeGrants in
 * internal/gateway/handler.go, TestNarrowedCustomerlessDeveloperIsNotPlatformWide).
 * Its reason names what to do instead, so the console has to show it as sent;
 * both controls used to fail silently — the checkbox stayed checked, the model
 * stayed listed, and nothing said why.
 *
 * The admin endpoints are mocked: the point is what the client does with a
 * refusal.
 */
import { test, expect, type Page } from "@playwright/test";

const SELF_ID = "11111111-1111-1111-1111-111111111111";
const USER_ID = "22222222-2222-2222-2222-222222222222";
const TENANT_ID = "33333333-3333-3333-3333-333333333333";
const APP_ID = "44444444-4444-4444-4444-444444444444";
const MODEL_ID = "55555555-5555-5555-5555-555555555555";

const REVOKE_REFUSAL = "narrow@example.com belongs to no tenant and holds developer without a workspace, and this is the last " +
  "application or model it is limited to: without it the account would be a builder of every tenant. " +
  "Revoke its developer role instead, or ask a platform admin.";
const DELETE_REFUSAL = "narrow@example.com belongs to no tenant and holds developer without a workspace, and this is the last " +
  "application or model it is limited to: without it the account would be a builder of every tenant. " +
  "Revoke that account's developer role first (Users), or ask a platform admin to delete it.";

async function openConsole(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem("dev_persona", "platform_admin");
  });
  const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
  // Anything not mocked below answers empty rather than reaching a gateway.
  await page.route((url) => url.pathname.startsWith("/api/"), (route) =>
    route.fulfill(json(route.request().method() === "GET" ? [] : {})));
  const me = { user_id: SELF_ID, email: "admin@example.com", display_name: "Admin", roles: ["platform_admin"] };
  await page.route("**/api/me", (route) => route.fulfill(json(me)));
  await page.route("**/api/admin/me", (route) => route.fulfill(json(me)));
  await page.route("**/api/admin/tenants", (route) => route.fulfill(json([{
    id: TENANT_ID, name: "Acme", plan: "enterprise", created_at: "2026-09-01T00:00:00Z",
    applications: [{
      id: APP_ID, name: "Planning App", mode: "planning", status: "active",
      models: [{ id: MODEL_ID, name: "Budget Model", storage_type: "oltp", active_revision: null, revisions: [] }],
    }],
  }])));
  await page.route("**/api/admin/users", (route) => route.fulfill(json([
    {
      id: SELF_ID, email: "admin@example.com", display_name: "Admin", created_at: "2026-09-01T00:00:00Z",
      assignments: [{ role: "platform_admin", workspace_id: "", workspace_name: "", customer_name: "" }], app_ids: [], model_ids: [],
    },
    {
      id: USER_ID, email: "narrow@example.com", display_name: "Narrow Dev", created_at: "2026-09-02T00:00:00Z",
      assignments: [{ role: "developer", workspace_id: "", workspace_name: "", customer_name: "" }],
      app_ids: [APP_ID], model_ids: [],
    },
  ])));
  await page.route(`**/api/admin/users/${USER_ID}/access/apps/${APP_ID}`, (route) =>
    route.fulfill(json({ error: REVOKE_REFUSAL }, 403)));
  await page.route(`**/api/admin/models/${MODEL_ID}`, (route) =>
    route.request().method() === "DELETE" ? route.fulfill(json({ error: DELETE_REFUSAL }, 409)) : route.fallback());
  await page.goto("/");
}

test("a refused revoke of application access says why, under that account", async ({ page }) => {
  await openConsole(page);
  await page.getByRole("button", { name: "Users" }).click();
  await page.getByLabel("Edit Narrow Dev").click();
  await page.getByRole("checkbox", { name: /Planning App/ }).click();
  await expect(page.getByRole("alert").filter({ hasText: "Revoke its developer role instead" })).toBeVisible();
});

test("a refused model delete says why", async ({ page }) => {
  await openConsole(page);
  await page.getByRole("button", { name: "Applications" }).click();
  await page.getByLabel("Delete model Budget Model").click();
  await page.getByRole("button", { name: "Delete model", exact: true }).click();
  await expect(page.getByRole("alert").filter({ hasText: "Revoke that account's developer role first" })).toBeVisible();
});
