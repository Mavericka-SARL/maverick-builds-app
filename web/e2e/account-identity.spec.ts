/**
 * Every console must answer "which account am I operating as" without opening
 * anything, and offer a way to stop being that account.
 *
 * The identity line lives in the context bar; the address, the full role list
 * and Sign out live behind the account button beside the notification bell.
 * The split exists so the same three facts are not printed twice on screen —
 * they used to be, in the context bar and again in the sidebar.
 */
import { test, expect, type Page } from "@playwright/test";

const PERSONAS = ["platform_admin", "developer", "finance", "dept_head"];

async function open(page: Page, persona: string) {
  await page.addInitScript((p) => localStorage.setItem("dev_persona", p), persona);
  const actor = {
    user_id: "00000000-0000-0000-0000-000000000009",
    email: "signed.in@example.com",
    display_name: "Signed In Person",
    roles: [persona === "finance" ? "business_admin" : persona === "dept_head" ? "business_user" : persona],
  };
  // UserMenu reads /api/me; the platform console's own role gate reads
  // /api/admin/me and drives its context bar from the same object.
  for (const path of ["**/api/me", "**/api/admin/me"]) {
    await page.route(path, (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(actor) }),
    );
  }
  await page.goto("/");
  await expect(page.locator(".mvx-app-shell__brand")).toBeVisible({ timeout: 15_000 });
}

const trigger = (page: Page) => page.getByRole("button", { name: /^Account:/ });

for (const persona of PERSONAS) {
  test(`${persona}: identity lives in the account button, not the context bar`, async ({ page }) => {
    await open(page, persona);
    // The sidebar heading already names the console (which is the role) and
    // the account button carries the name; a third copy in the context bar
    // was pure duplication.
    await expect(page.locator(".mvx-context-bar")).not.toContainText("Signed In Person");
    await expect(trigger(page)).toBeVisible();
  });
}

test("the account menu holds the address, roles and sign out", async ({ page }) => {
  await open(page, "platform_admin");

  // Closed by default: nothing duplicated on screen.
  await expect(page.getByRole("menuitem", { name: "Sign out" })).toHaveCount(0);
  await expect(page.getByText("signed.in@example.com")).toHaveCount(0);

  await trigger(page).click();
  await expect(page.getByText("signed.in@example.com")).toBeVisible();
  await expect(page.getByRole("menuitem", { name: "Sign out" })).toBeVisible();
});

test("the account menu is actually on screen, not clipped by the context bar", async ({ page }) => {
  // toBeVisible() only asks for a non-empty box, which a clipped element still
  // has: the first version of this panel was absolutely positioned inside
  // .mvx-context-bar, whose overflow-x: auto clips on both axes. It reported
  // visible while elementFromPoint over it returned the page behind, so every
  // click went through and the button looked dead. Hit-test instead.
  await open(page, "platform_admin");
  await trigger(page).click();

  const popover = page.locator(".mvx-user-menu__popover");
  await expect(popover).toBeVisible();

  const hit = await popover.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const atTop = document.elementFromPoint(r.left + r.width / 2, r.top + 20);
    const atBottom = document.elementFromPoint(r.left + r.width / 2, r.bottom - 12);
    return { top: el.contains(atTop), bottom: el.contains(atBottom) };
  });
  expect(hit, "panel is covered or clipped — clicks will pass through it").toEqual({ top: true, bottom: true });

  // The end of the real path: Playwright's actionability requires the element
  // to receive the event, so this fails outright on a clipped panel.
  await popover.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(popover).toHaveCount(0);
});

test("the account menu closes on Escape and on an outside click", async ({ page }) => {
  await open(page, "platform_admin");
  const signOut = page.getByRole("menuitem", { name: "Sign out" });

  await trigger(page).click();
  await expect(signOut).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(signOut).toHaveCount(0);

  await trigger(page).click();
  await expect(signOut).toBeVisible();
  await page.locator(".mvx-app-shell__nav").click({ position: { x: 5, y: 5 } });
  await expect(signOut).toHaveCount(0);
});

test("the identity is not repeated in the sidebar", async ({ page }) => {
  await open(page, "platform_admin");
  // The block that used to sit here duplicated the context bar verbatim.
  const footer = page.locator(".mvx-app-shell__footer");
  await expect(footer.getByText("signed.in@example.com")).toHaveCount(0);
  await expect(footer.getByRole("button", { name: "Sign out" })).toHaveCount(0);
});

test("a disabled danger action is not still painted red", async ({ page }) => {
  // The self-delete guard renders the trash icon disabled. Opacity alone left
  // it reading as an armed red button next to an enabled one.
  await page.addInitScript(() => localStorage.setItem("dev_persona", "platform_admin"));
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const SELF = "11111111-1111-1111-1111-111111111111";
  const actor = { user_id: SELF, email: "a@b.c", display_name: "Admin", roles: ["platform_admin"] };
  for (const p of ["**/api/me", "**/api/admin/me"]) await page.route(p, (r) => r.fulfill(json(actor)));
  await page.route("**/api/admin/tenants", (r) => r.fulfill(json([])));
  await page.route("**/api/admin/workspaces", (r) => r.fulfill(json([])));
  await page.route("**/api/admin/audit**", (r) => r.fulfill(json([])));
  await page.route("**/api/admin/users", (r) => r.fulfill(json([
    { id: SELF, email: "a@b.c", display_name: "Admin", created_at: "2026-08-15T00:00:00Z",
      assignments: [{ role: "platform_admin", workspace_id: "", workspace_name: "", customer_name: "" }], app_ids: [], model_ids: [] },
    { id: "22222222-2222-2222-2222-222222222222", email: "d@e.f", display_name: "Other", created_at: "2026-08-16T00:00:00Z",
      assignments: [{ role: "developer", workspace_id: "", workspace_name: "", customer_name: "" }], app_ids: [], model_ids: [] },
  ])));

  await page.goto("/");
  await page.getByRole("button", { name: "Users" }).click();
  await expect(page.getByText("d@e.f")).toBeVisible({ timeout: 15_000 });

  const disabled = page.getByLabel("You cannot delete your own account");
  const enabled = page.getByLabel("Delete Other");
  const colourOf = (l: typeof disabled) => l.evaluate((el) => getComputedStyle(el).color);

  await expect(disabled).toBeDisabled();
  expect(await colourOf(disabled)).not.toBe(await colourOf(enabled));
});

/**
 * roleIsAssignableBy has always let a developer assign business_admin and
 * business_user, but the invite form offered only PLATFORM_ROLES intersected
 * with that set — which for a developer is empty, so the dropdown read
 * "None" and there was no way to give an invited user any role at all.
 */
test("a developer can pick a business role and scope it to a workspace", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("dev_persona", "developer"));
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const actor = { user_id: "dev-1", email: "dev@example.com", display_name: "Dev", roles: ["developer"] };
  for (const p of ["**/api/me", "**/api/admin/me"]) await page.route(p, (r) => r.fulfill(json(actor)));
  await page.route("**/api/admin/users", (r) => r.fulfill(json([])));
  const tenants = [{ id: "t1", name: "Acme", plan: "enterprise", applications: [] }];
  await page.route("**/api/admin/tenants", (r) => r.fulfill(json(tenants)));
  // The developer console sources its tenant list from its own endpoint, not
  // the admin one — without this the Users tab never leaves "Loading…".
  await page.route("**/api/developer/applications", (r) => r.fulfill(json(tenants)));
  await page.route("**/api/admin/workspaces", (r) => r.fulfill(json([
    { id: "ws-1", name: "Planning", customer_name: "Acme", customer_id: "t1" },
  ])));

  await page.goto("/");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Users" }).click();
  await page.getByRole("button", { name: "Invite user" }).click();

  const role = page.getByLabel("Initial Role");
  await expect(role).toBeVisible();
  await role.selectOption("business_user");

  // A business role is inert without a workspace, so the form asks for one
  // and will not submit until it has it.
  const workspace = page.getByLabel("Workspace");
  await expect(workspace).toBeVisible();
  await page.getByLabel("Email").fill("jane@acme.com");
  // Both names, because Keycloak's realm requires them: an account missing
  // either meets the invited user with "Update Account Information" before
  // they can finish signing in.
  await page.getByLabel("First name").fill("Jane");
  await page.getByLabel("Last name").fill("Smith");
  await expect(page.getByRole("button", { name: "Create user" })).toBeDisabled();

  await workspace.selectOption("ws-1");
  await expect(page.getByRole("button", { name: "Create user" })).toBeEnabled();
});
