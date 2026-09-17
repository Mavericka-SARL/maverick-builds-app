/**
 * The Users screen offers an administrator two ways to end their own access:
 * the delete button on their own row, and the ✕ on their own role chip. The
 * server refuses both (see the DELETE branches in internal/gateway/handler.go
 * and TestCannotDeleteOwnAccount / TestCannotRemoveOwnLastAdminRole); these
 * tests cover the other half — that the controls say so up front instead of
 * failing on click, and that currentUserId actually reaches UsersPanel, which
 * is the wiring most likely to break silently.
 *
 * The admin endpoints are mocked rather than seeded: the point is the client's
 * behaviour given a known "who am I" answer.
 */
import { test, expect, type Page } from "@playwright/test";

const SELF_ID = "11111111-1111-1111-1111-111111111111";
const OTHER_ID = "22222222-2222-2222-2222-222222222222";

const platformGrant = { role: "platform_admin", workspace_id: "", workspace_name: "", customer_name: "" };

async function openUsersTab(page: Page, selfAssignments = [platformGrant]) {
  await page.addInitScript(() => {
    localStorage.setItem("dev_persona", "platform_admin");
  });

  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

  await page.route("**/api/me", (route) => route.fulfill(json({
    user_id: SELF_ID, email: "admin@example.com", display_name: "Admin", roles: ["platform_admin"],
  })));
  await page.route("**/api/admin/me", (route) => route.fulfill(json({
    user_id: SELF_ID, email: "admin@example.com", display_name: "Admin", roles: ["platform_admin"],
  })));
  await page.route("**/api/admin/tenants", (route) => route.fulfill(json([])));
  await page.route("**/api/admin/workspaces", (route) => route.fulfill(json([])));
  await page.route("**/api/admin/audit**", (route) => route.fulfill(json([])));
  await page.route("**/api/admin/users", (route) => route.fulfill(json([
    {
      id: SELF_ID, email: "admin@example.com", display_name: "Admin",
      created_at: "2026-08-15T00:00:00Z", assignments: selfAssignments, app_ids: [], model_ids: [],
    },
    {
      id: OTHER_ID, email: "someone@example.com", display_name: "Someone Else",
      created_at: "2026-08-16T00:00:00Z",
      assignments: [{ role: "developer", workspace_id: "", workspace_name: "", customer_name: "" }],
      app_ids: [], model_ids: [],
    },
  ])));

  await page.goto("/");
  await page.getByRole("button", { name: "Users" }).click();
  await expect(page.getByText("someone@example.com")).toBeVisible({ timeout: 15_000 });
}

test("the delete button is inert on your own row and live on everyone else's", async ({ page }) => {
  await openUsersTab(page);

  // Left visible rather than hidden: a missing button just reads as a bug.
  const own = page.getByLabel("You cannot delete your own account");
  await expect(own).toBeVisible();
  await expect(own).toBeDisabled();

  await expect(page.getByLabel("Delete Someone Else")).toBeEnabled();
});

test("the ✕ on your own last admin role explains why it is unavailable", async ({ page }) => {
  await openUsersTab(page);
  await page.getByLabel("Edit Admin").click();

  const chip = page.getByLabel(/another administrator has to remove it/);
  await expect(chip).toBeVisible();
  await expect(chip).toBeDisabled();
});

test("a second admin role makes your own platform_admin chip removable again", async ({ page }) => {
  // Holding tenant_admin as well, user administration survives the removal —
  // so the client must not block it either.
  await openUsersTab(page, [
    platformGrant,
    { role: "tenant_admin", workspace_id: "", workspace_name: "", customer_name: "" },
  ]);
  await page.getByLabel("Edit Admin").click();

  await expect(page.getByLabel(/another administrator has to remove it/)).toHaveCount(0);
  await expect(page.getByLabel("Remove platform_admin")).toBeEnabled();
});
