/**
 * A tenant with a database of its own keeps its people there. The platform
 * admin's Users tab is one list across every database, each row naming the
 * one it lives in; "People of" narrows it, and what is done to a row — or an
 * invitation into a workspace — is addressed to that row's or workspace's
 * database (X-Tenant-Id). It used to show the control plane's people only,
 * and then one tenant's at a time.
 */
import { test, expect, type Page, type Request } from "@playwright/test";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
const GLOBEX = "22222222-2222-2222-2222-222222222222";

async function open(page: Page, tenants: object[]) {
  await page.addInitScript(() => localStorage.setItem("dev_persona", "platform_admin"));
  const actor = { user_id: "pa-1", email: "pa@example.com", display_name: "Pat Admin", roles: ["platform_admin"] };
  for (const p of ["**/api/me", "**/api/admin/me"]) await page.route(p, (r) => r.fulfill(json(actor)));
  await page.route("**/api/admin/tenants", (r) => r.fulfill(json(tenants)));
  await page.route("**/api/admin/audit**", (r) => r.fulfill(json([])));
  const invited: Request[] = [];
  await page.route("**/api/admin/users", (r) => {
    if (r.request().method() === "POST") {
      invited.push(r.request());
      return r.fulfill(json({ id: "new-1", status: "created", invited: true }));
    }
    const row = (id: string, email: string, display_name: string, tenant_id: string, tenant_name = "") => ({
      id, email, display_name, tenant_id, tenant_name, created_at: "2026-09-30T00:00:00Z", assignments: [], home_tenant: "own",
      permissions: { rename: true, delete: true, disable: true, reinvite: true, remove_from_tenant: false }, grantable_roles: [],
    });
    const dedicated = tenants.some((t) => (t as { dedicated?: boolean }).dedicated);
    return r.fulfill(json(dedicated
      ? [row("u-cp", "cp@shared.test", "Shared Person", "control-plane"), row("u-gil", "gil@globex.test", "Gil Globex", GLOBEX, "Globex")]
      : [row("u-cp", "cp@shared.test", "Shared Person", "")]));
  });
  await page.route("**/api/admin/workspaces", (r) => r.fulfill(json([
    { id: "ws-s", name: "Shared Default", customer_name: "Shared Co", customer_id: "t-shared", tenant_id: "control-plane" },
    { id: "ws-g", name: "Globex Default", customer_name: "Globex", customer_id: GLOBEX, tenant_id: GLOBEX },
  ])));
  await page.goto("/");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Users" }).click();
  return invited;
}

test("the platform admin sees everyone, and invites into a tenant's own database", async ({ page }) => {
  const invited = await open(page, [
    { id: "t-shared", name: "Shared Co", plan: "starter", created_at: "2026-09-01T00:00:00Z", applications: [] },
    { id: GLOBEX, name: "Globex", plan: "enterprise", created_at: "2026-09-02T00:00:00Z", applications: [], dedicated: true },
  ]);
  // One list: both databases' people, each naming where it lives.
  await expect(page.getByText("cp@shared.test")).toBeVisible();
  await expect(page.getByText("gil@globex.test")).toBeVisible();
  await expect(page.getByTestId("user-database").filter({ hasText: "Control plane" })).toHaveCount(1);
  await expect(page.getByTestId("user-database").filter({ hasText: "Globex" })).toHaveCount(1);

  // Narrowed to Globex.
  await page.getByLabel("People of").selectOption(GLOBEX);
  await expect(page.getByText("gil@globex.test")).toBeVisible();
  await expect(page.getByText("cp@shared.test")).toHaveCount(0);

  // An invitation into Globex's workspace is addressed to Globex's database.
  await page.getByRole("button", { name: "Invite user" }).click();
  await page.getByLabel("Email").fill("new@globex.test");
  await page.getByLabel("First name").fill("New");
  await page.getByLabel("Last name").fill("Person");
  await page.getByLabel("Initial Role").selectOption("business_user");
  await page.getByLabel("Workspace").selectOption("ws-g");
  await page.getByRole("button", { name: "Create user" }).click();

  await expect.poll(() => invited.length).toBe(1);
  expect(invited[0].headers()["x-tenant-id"]).toBe(GLOBEX);
  expect(invited[0].postDataJSON()).toMatchObject({ email: "new@globex.test", role: "business_user", workspace_id: "ws-g" });
});

test("without a tenant that has its own database there is nothing to choose", async ({ page }) => {
  await open(page, [{ id: "t-shared", name: "Shared Co", plan: "starter", created_at: "2026-09-01T00:00:00Z", applications: [] }]);
  await expect(page.getByText("cp@shared.test")).toBeVisible();
  await expect(page.getByTestId("users-scope")).toHaveCount(0);
});
