/**
 * A tenant created by a platform administrator who does not take it as their
 * own starts with its first administrator: invited into the new tenant
 * (X-Tenant-Id) as tenant_admin of its default workspace. With a database
 * per tenant a platform administrator never holds a tenant's roles (owner
 * decision 2026-11-09). Mocked: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("Create tenant invites its first administrator into the new tenant", async ({ page }) => {
  await mockApi(page);
  const json = (status: number, body: unknown) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
  const invited: { tenant?: string; body?: Record<string, unknown> } = {};
  await page.route((url) => url.pathname === "/api/admin/tenants", (route) =>
    route.request().method() === "POST"
      ? route.fulfill(json(200, { id: "t-new", workspace_id: "ws-new" }))
      : route.fallback());
  await page.route((url) => url.pathname === "/api/admin/users", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    invited.tenant = route.request().headers()["x-tenant-id"];
    invited.body = route.request().postDataJSON();
    return route.fulfill(json(200, { id: "u-new", status: "invited", invited: true }));
  });

  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Applications", exact: true }).first().click();
  await page.getByRole("button", { name: "New tenant" }).first().click();
  await page.getByPlaceholder("Tenant name (e.g. Acme Corp)").fill("Northwind");
  const own = page.getByLabel("Make it my own, with every role");
  if (await own.count()) await own.uncheck();
  await page.getByLabel("First administrator's e-mail").fill("olga@northwind.example");
  await page.getByLabel("First administrator's first name").fill("Olga");
  await page.getByLabel("First administrator's last name").fill("Owner");
  await page.getByRole("button", { name: "Create tenant" }).click();

  await expect(page.getByText("olga@northwind.example is the tenant's administrator and was sent an invitation.")).toBeVisible();
  expect(invited.tenant).toBe("t-new");
  expect(invited.body).toEqual({ email: "olga@northwind.example", first_name: "Olga", last_name: "Owner", role: "tenant_admin", workspace_id: "ws-new" });
});
