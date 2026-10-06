/**
 * A tenant admin changes only its own tenant's accounts (user decision
 * 2026-09-30). The server computes, per listed account, what the signed-in
 * administrator may do to it as a whole ("permissions") and whose account it
 * is ("home_tenant"); the Users screen offers exactly that and nothing it
 * would refuse:
 *
 *  - rename / delete / re-invite only where permissions allow, and none of
 *    them when a server older than the field sends no permissions at all;
 *  - an account of another organisation, or of none, carries a note saying
 *    which, and a "Remove from this tenant" action
 *    (DELETE /api/admin/users/{id}/tenant-access), asked for first;
 *  - Invite user confirms the same way whether the address already had an
 *    account (then only given the role, in a workspace) or not;
 *  - for anyone but a platform admin, Invite user asks for a role and a
 *    workspace before it can be sent, for every address alike (user decision
 *    2026-09-30); a platform admin keeps inviting with neither;
 *  - a revoke, a removal from the tenant, or an application/model delete
 *    that also revoked a narrowed developer's grant says so — from the
 *    reply's revoked list, or its message when it sends one — above the list,
 *    since the account then leaves it.
 *
 * The admin endpoints are mocked, following what the gateway answers: an
 * account leaves GET /api/admin/users once nothing in the tenant lists it,
 * and anyone but a platform admin inviting without a role and a workspace is
 * refused, the same way whether or not the address already has an account.
 */
import { test, expect, type Page, type Request } from "@playwright/test";

const SELF_ID = "11111111-1111-1111-1111-111111111111";
const OWN_ID = "22222222-2222-2222-2222-222222222222";
const FOREIGN_ID = "33333333-3333-3333-3333-333333333333";
const NONE_ID = "44444444-4444-4444-4444-444444444444";
const LEGACY_ID = "55555555-5555-5555-5555-555555555555";
const TENANT_ID = "66666666-6666-6666-6666-666666666666";
const WS_ID = "77777777-7777-7777-7777-777777777777";
const APP_ID = "88888888-8888-8888-8888-888888888888";
const MODEL_ID = "99999999-9999-9999-9999-999999999999";

const OTHER_NOTE = "Member of another organisation — you can only remove their access here";
const NONE_NOTE = "Not a member of any organisation — you can only remove their access here";
const REVOKED_MESSAGE = "nora@nowhere.test belonged to no tenant and held developer without a workspace, limited to this " +
  "application only: its developer role was revoked too, so it builds nothing.";
// What removeGrants actually answers: the grants it took with the removal.
const REVOKED = [{ user_id: NONE_ID, email: "nora@nowhere.test", role: "developer" }];
const REVOKED_NOTICE = "Also revoked developer from nora@nowhere.test. That account belongs to no tenant and was limited " +
  "to what was removed: left in place, the grant would have made it a builder of every tenant.";

const unscoped = (role: string) => ({ role, workspace_id: "", workspace_name: "", customer_name: "" });
const inWorkspace = (role: string) => ({ role, workspace_id: WS_ID, workspace_name: "Planning", customer_name: "Acme" });
const allowed = { rename: true, delete: true, disable: true, reinvite: true, remove_from_tenant: false };
const foreignOnly = { rename: false, delete: false, disable: false, reinvite: false, remove_from_tenant: true };

const users = [
  {
    id: SELF_ID, email: "admin@acme.test", display_name: "Ada Admin", created_at: "2026-09-01T00:00:00Z",
    assignments: [unscoped("tenant_admin")], app_ids: [], model_ids: [],
    home_tenant: "own", permissions: { ...allowed, delete: false },
  },
  {
    id: OWN_ID, email: "olivia@acme.test", display_name: "Olivia Own", created_at: "2026-09-02T00:00:00Z",
    assignments: [inWorkspace("business_user")], app_ids: [], model_ids: [],
    home_tenant: "own", permissions: allowed,
  },
  {
    id: FOREIGN_ID, email: "fred@other.test", display_name: "Fred Foreign", created_at: "2026-09-03T00:00:00Z",
    assignments: [unscoped("developer"), inWorkspace("business_user")], app_ids: [], model_ids: [],
    home_tenant: "other", permissions: foreignOnly,
  },
  {
    id: NONE_ID, email: "nora@nowhere.test", display_name: "Nora None", created_at: "2026-09-04T00:00:00Z",
    assignments: [unscoped("developer")], app_ids: [APP_ID], model_ids: [],
    home_tenant: "none", permissions: foreignOnly,
  },
  {
    // A server predating the fields: no permissions, no home_tenant.
    id: LEGACY_ID, email: "leo@acme.test", display_name: "Leo Legacy", created_at: "2026-09-05T00:00:00Z",
    assignments: [inWorkspace("business_user")], app_ids: [], model_ids: [],
  },
];

type Json = Record<string, unknown>;

type Replies = {
  invite?: Json[]; revoke?: Json; tenantAccess?: Json; deleteModel?: Json; deleteApp?: Json; users?: Json[];
  /** Who is signed in; a tenant admin unless said otherwise. */
  persona?: "tenant_admin" | "platform_admin";
  /** /api/admin/me answers only once this settles (the caller's roles still loading). */
  holdAdminMe?: Promise<void>;
  /** Filled by openConsole: relist() lists every account again, as after a re-grant. */
  control?: { relist?: () => void };
};

// What the gateway answers anyone but a platform admin inviting without both —
// before it looks the address up, so alike for new and existing addresses.
const NEEDS_ROLE_AND_WORKSPACE = "choose a role and a workspace: people are always added to a workspace";
const WORKSPACE_HINT = "People are always added to a workspace";

async function openConsole(page: Page, replies: Replies = {}) {
  const calls: Request[] = [];
  // The list as the gateway would answer it now: a removal can take away
  // what listed an account in this tenant at all.
  let listed: Json[] = [...(replies.users ?? users)];
  const unlist = (ids: string[]) => { listed = listed.filter((u) => !ids.includes(u.id as string)); };
  if (replies.control) replies.control.relist = () => { listed = [...(replies.users ?? users)]; };
  const revokedIds = (reply: Json | undefined) => ((reply?.revoked as { user_id: string }[] | undefined) ?? []).map((g) => g.user_id);
  const persona = replies.persona ?? "tenant_admin";
  await page.addInitScript((p) => localStorage.setItem("dev_persona", p), persona);
  const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
  // Anything not mocked below answers empty rather than reaching a gateway.
  await page.route((url) => url.pathname.startsWith("/api/"), (route) =>
    route.fulfill(json(route.request().method() === "GET" ? [] : {})));
  const me = { user_id: SELF_ID, email: "admin@acme.test", display_name: "Ada Admin", roles: [persona] };
  await page.route("**/api/me", (route) => route.fulfill(json(me)));
  await page.route("**/api/admin/me", async (route) => {
    await replies.holdAdminMe;
    await route.fulfill(json(me));
  });
  await page.route("**/api/admin/tenants", (route) => route.fulfill(json([{
    id: TENANT_ID, name: "Acme", plan: "enterprise", created_at: "2026-09-01T00:00:00Z",
    applications: [{
      id: APP_ID, name: "Planning App", mode: "planning", status: "active",
      models: [{ id: MODEL_ID, name: "Budget Model", storage_type: "oltp", active_revision: null, revisions: [] }],
    }],
  }])));
  await page.route("**/api/admin/workspaces", (route) => route.fulfill(json([
    { id: WS_ID, name: "Planning", customer_name: "Acme", customer_id: TENANT_ID },
  ])));
  const invites = [...(replies.invite ?? [])];
  await page.route("**/api/admin/users", (route) => {
    if (route.request().method() === "POST") {
      calls.push(route.request());
      const body = route.request().postDataJSON() as { email: string; role?: string; workspace_id?: string };
      if (persona !== "platform_admin" && (!body.role || !body.workspace_id)) {
        return route.fulfill(json({ error: NEEDS_ROLE_AND_WORKSPACE }, 400));
      }
      return route.fulfill(json(invites.shift() ?? { id: "new-id", status: "created", invited: true }));
    }
    return route.fulfill(json(listed));
  });
  await page.route("**/api/admin/users/*/**", (route) => {
    calls.push(route.request());
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/tenant-access")) {
      const reply = replies.tenantAccess ?? { status: "removed" };
      unlist([path.split("/")[4], ...revokedIds(reply)]);
      return route.fulfill(json(reply));
    }
    if (path.endsWith("/invite")) return route.fulfill(json({ status: "invited", email: "olivia@acme.test" }));
    if (path.includes("/access/apps/")) {
      const reply = replies.revoke ?? { status: "revoked" };
      // Its last narrowing grant and its developer grant gone, a tenant-less
      // account holds nothing in this tenant, and is no longer listed.
      unlist(revokedIds(reply));
      return route.fulfill(json(reply));
    }
    return route.fulfill(json({ status: "ok" }));
  });
  await page.route(`**/api/admin/models/${MODEL_ID}`, (route) =>
    route.request().method() === "DELETE" ? route.fulfill(json(replies.deleteModel ?? { status: "deleted" })) : route.fallback());
  await page.route(`**/api/admin/applications/${APP_ID}`, (route) =>
    route.request().method() === "DELETE" ? route.fulfill(json(replies.deleteApp ?? { status: "deleted" })) : route.fallback());
  await page.goto("/");
  return calls;
}

const nav = (page: Page, name: string) =>
  page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name, exact: true });

async function openUsers(page: Page) {
  await nav(page, "Users").click();
  await expect(page.getByText("fred@other.test")).toBeVisible({ timeout: 15_000 });
}

test("account-level actions follow the server's permissions", async ({ page }) => {
  await openConsole(page);
  await openUsers(page);

  // Own tenant's account: delete and re-invite, and no "remove from tenant".
  await expect(page.getByLabel("Delete Olivia Own")).toBeEnabled();
  await expect(page.getByLabel("Resend invitation to Olivia Own")).toBeVisible();
  await expect(page.getByLabel("Remove Olivia Own from this tenant")).toHaveCount(0);

  // Another organisation's account, and one with none: only their access here.
  for (const [name, note] of [["Fred Foreign", OTHER_NOTE], ["Nora None", NONE_NOTE]]) {
    await expect(page.getByLabel(`Delete ${name}`)).toHaveCount(0);
    await expect(page.getByLabel(`Resend invitation to ${name}`)).toHaveCount(0);
    await expect(page.getByLabel(`Remove ${name} from this tenant`)).toBeVisible();
    await expect(page.getByRole("row").filter({ hasText: name }).getByText(note, { exact: true })).toBeVisible();
  }
  await expect(page.getByRole("row").filter({ hasText: "Olivia Own" }).getByText(/organisation/)).toHaveCount(0);

  // A server that sends no permissions gets no account-level action at all;
  // roles are still managed from Edit.
  await expect(page.getByLabel("Delete Leo Legacy")).toHaveCount(0);
  await expect(page.getByLabel("Resend invitation to Leo Legacy")).toHaveCount(0);
  await expect(page.getByLabel("Remove Leo Legacy from this tenant")).toHaveCount(0);
  await expect(page.getByLabel("Edit Leo Legacy")).toBeVisible();

  // Your own row keeps its inert, explained delete.
  await expect(page.getByLabel("You cannot delete your own account")).toBeDisabled();
});

test("another organisation's account cannot be renamed or re-roled at home", async ({ page }) => {
  await openConsole(page);
  await openUsers(page);

  await page.getByLabel("Edit Fred Foreign").click();
  await expect(page.getByPlaceholder("display name")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Save" })).toHaveCount(0);
  const editing = page.getByRole("row").filter({ has: page.getByRole("button", { name: "Close" }) });
  await expect(editing.getByText(OTHER_NOTE, { exact: true })).toBeVisible();
  // developer with no workspace is his own organisation's grant, not ours to take.
  await expect(page.getByLabel("Remove developer")).toHaveCount(0);
  await expect(page.getByLabel("Add platform role")).toHaveCount(0);
  // The role he holds in our workspace is ours to remove.
  await expect(page.getByLabel("Remove business_user")).toBeEnabled();
  await page.getByRole("button", { name: "Close" }).click();

  await page.getByLabel("Edit Olivia Own").click();
  await expect(page.getByPlaceholder("display name")).toHaveValue("Olivia Own");
  await expect(page.getByRole("button", { name: "Save" })).toBeVisible();
});

test("Remove from this tenant asks first, then removes only the tenant access", async ({ page }) => {
  const calls = await openConsole(page);
  await openUsers(page);

  await page.getByLabel("Remove Fred Foreign from this tenant").click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog.getByText("Remove from this tenant?")).toBeVisible();
  await expect(dialog.getByText(/Their account itself is not changed/)).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  expect(calls.filter((r) => r.method() === "DELETE")).toHaveLength(0);

  await page.getByLabel("Remove Fred Foreign from this tenant").click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Remove from this tenant" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Removed Fred Foreign from this tenant." })).toBeVisible();
  const deletes = calls.filter((r) => r.method() === "DELETE").map((r) => new URL(r.url()).pathname);
  expect(deletes).toEqual([`/api/admin/users/${FOREIGN_ID}/tenant-access`]);
});

test("Remove from this tenant says when it also took a narrowed developer's role", async ({ page }) => {
  await openConsole(page, { tenantAccess: { status: "removed", revoked: REVOKED } });
  await openUsers(page);

  await page.getByLabel("Remove Nora None from this tenant").click();
  const dialog = page.getByRole("alertdialog");
  // Her developer role has no organisation and is narrowed by grants here:
  // the dialog does not promise the account is left as it is.
  await expect(dialog.getByText(/that developer role is revoked too/)).toBeVisible();
  await expect(dialog.getByText(/Their account itself is not changed/)).toHaveCount(0);
  await dialog.getByRole("button", { name: "Remove from this tenant" }).click();

  await expect(page.getByRole("status").filter({ hasText: `Removed Nora None from this tenant. ${REVOKED_NOTICE}` })).toBeVisible();
  await expect(page.getByRole("row").filter({ hasText: "Nora None" })).toHaveCount(0);
});

test("the foreign-account note says whose it is, and offers removal only when it is offered", async ({ page }) => {
  const noRemove = { ...foreignOnly, remove_from_tenant: false };
  await openConsole(page, { users: users.map((u) =>
    u.id === FOREIGN_ID || u.id === NONE_ID ? { ...u, permissions: noRemove } : u) });
  await openUsers(page);
  const fred = page.getByRole("row").filter({ hasText: "Fred Foreign" });
  const nora = page.getByRole("row").filter({ hasText: "Nora None" });
  await expect(fred.getByText("Member of another organisation", { exact: true })).toBeVisible();
  await expect(nora.getByText("Not a member of any organisation", { exact: true })).toBeVisible();
  await expect(page.getByText(/you can only remove their access here/)).toHaveCount(0);
  await expect(page.getByLabel("Remove Fred Foreign from this tenant")).toHaveCount(0);
  await expect(page.getByLabel("Remove Nora None from this tenant")).toHaveCount(0);
});

test("Resend invitation confirms where it went", async ({ page }) => {
  const calls = await openConsole(page);
  await openUsers(page);
  await page.getByLabel("Resend invitation to Olivia Own").click();
  await expect(page.getByRole("status").filter({ hasText: "Invitation sent to olivia@acme.test." })).toBeVisible();
  expect(calls.map((r) => `${r.method()} ${new URL(r.url()).pathname}`)).toContain(`POST /api/admin/users/${OWN_ID}/invite`);
});

test("Invite user confirms the same whether the address already had an account", async ({ page }) => {
  // The first reply is a brand-new account's, the second one an existing
  // account's that was only given the role: the confirmation must not tell
  // them apart.
  await openConsole(page, { invite: [
    { id: "new-id", status: "created", invited: true },
    { id: FOREIGN_ID, status: "created", invited: false },
  ] });
  await openUsers(page);

  // Both with a role in a workspace: an existing address is only ever added
  // inside one, and the gateway refuses it without (see openConsole).
  const invite = async (email: string) => {
    await page.getByRole("button", { name: "Invite user" }).click();
    await page.getByLabel("Email").fill(email);
    await page.getByLabel("First name").fill("Jane");
    await page.getByLabel("Last name").fill("Smith");
    await page.getByLabel("Initial Role").selectOption("business_user");
    await page.getByLabel("Workspace").selectOption(WS_ID);
    await page.getByRole("button", { name: "Create user" }).click();
    const done = page.getByRole("status").filter({ hasText: email });
    await expect(done).toBeVisible();
    return (await done.innerText()).replace(email, "<email>");
  };
  const fresh = await invite("jane@new.test");
  const existing = await invite("fred@other.test");
  expect(fresh).toBe("Invited <email>. They will be notified of their access.");
  expect(existing).toBe(fresh);
});

test("a revoke that also took a developer grant says so, after the account has left the list", async ({ page }) => {
  await openConsole(page, { revoke: { status: "ok", revoked: REVOKED } });
  await openUsers(page);
  await page.getByLabel("Edit Nora None").click();
  await page.getByRole("checkbox", { name: /Planning App/ }).click();
  // With both grants gone nothing lists her in this tenant, so her row — and
  // the editor it held — leaves on the refetch; the notice stays.
  await expect(page.getByRole("row").filter({ hasText: "Nora None" })).toHaveCount(0);
  await expect(page.getByRole("status").filter({ hasText: REVOKED_NOTICE })).toBeVisible();
});

test("an account that left the list while being edited comes back closed", async ({ page }) => {
  const control: { relist?: () => void } = {};
  await openConsole(page, { revoke: { status: "ok", revoked: REVOKED }, control });
  await openUsers(page);
  await page.getByLabel("Edit Nora None").click();
  await page.getByRole("checkbox", { name: /Planning App/ }).click();
  await expect(page.getByRole("row").filter({ hasText: "Nora None" })).toHaveCount(0);
  // Listed again (a later grant elsewhere), and the list refetched by an
  // unrelated change — removing Fred: her row used to reopen in edit mode.
  control.relist!();
  await page.getByLabel("Remove Fred Foreign from this tenant").click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Remove from this tenant" }).click();
  await expect(page.getByRole("row").filter({ hasText: "Fred Foreign" })).toHaveCount(0);
  await expect(page.getByLabel("Edit Nora None")).toBeVisible();
});

test("the invite form says it is loading the caller's roles, then offers them", async ({ page }) => {
  let release!: () => void;
  await openConsole(page, { holdAdminMe: new Promise<void>((r) => { release = r; }) });
  await openUsers(page);
  await page.getByRole("button", { name: "Invite user" }).click();
  await expect(page.getByText("Loading the roles you can give…")).toBeVisible();
  await page.getByLabel("Email").fill("jane@new.test");
  await page.getByLabel("First name").fill("Jane");
  await page.getByLabel("Last name").fill("Smith");
  await expect(page.getByRole("button", { name: "Create user" })).toBeDisabled();
  release();
  await expect(page.getByLabel("Initial Role")).toBeVisible();
  await expect(page.getByText("Loading the roles you can give…")).toHaveCount(0);
});

test("a revoke that took nothing else says nothing more", async ({ page }) => {
  await openConsole(page, { revoke: { status: "ok" } });
  await openUsers(page);
  await page.getByLabel("Edit Nora None").click();
  const box = page.getByRole("checkbox", { name: /Planning App/ });
  const answered = page.waitForResponse((r) => r.url().includes("/access/apps/") && r.request().method() === "DELETE");
  await box.click();
  await answered;
  await expect(page.getByRole("status")).toHaveCount(0);
});

test("a model or application delete that also took a developer grant says so", async ({ page }) => {
  await openConsole(page, {
    deleteModel: { status: "deleted", revoked: REVOKED },
    deleteApp: { status: "deleted", message: `${REVOKED_MESSAGE} (application)`, revoked: REVOKED },
  });
  await nav(page, "Applications").click();

  await page.getByLabel("Delete model Budget Model").click();
  await page.getByRole("button", { name: "Delete model", exact: true }).click();
  await expect(page.getByRole("status").filter({ hasText: REVOKED_NOTICE })).toBeVisible();

  // A reply that words it itself is shown as sent.
  await page.getByLabel("Delete application Planning App").click();
  await page.getByRole("button", { name: "Delete application", exact: true }).click();
  await expect(page.getByRole("status").filter({ hasText: `${REVOKED_MESSAGE} (application)` })).toBeVisible();
});

test("a developer invite can be placed in a workspace, which is how an existing account is added", async ({ page }) => {
  // An address that already has an account is only ever given developer or
  // tenant_admin inside a workspace of the caller's tenant; the form has to
  // be able to say which.
  const calls = await openConsole(page);
  await openUsers(page);
  await page.getByRole("button", { name: "Invite user" }).click();
  await page.getByLabel("Email").fill("fred@other.test");
  await page.getByLabel("First name").fill("Fred");
  await page.getByLabel("Last name").fill("Foreign");
  await page.getByLabel("Initial Role").selectOption("developer");
  const workspace = page.getByLabel("Workspace");
  await expect(workspace).toHaveValue("");
  await workspace.selectOption(WS_ID);
  await page.getByRole("button", { name: "Create user" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Invited fred@other.test." })).toBeVisible();
  const post = calls.find((r) => r.method() === "POST" && new URL(r.url()).pathname === "/api/admin/users");
  expect(post?.postDataJSON()).toMatchObject({ email: "fred@other.test", role: "developer", workspace_id: WS_ID });
});

test("a tenant admin's invite asks for a role and a workspace before it can be sent", async ({ page }) => {
  // The gateway refuses anyone but a platform admin an invite without both,
  // for a new address and an existing one alike, so the answer never says
  // which it was; the form asks for both rather than offering "None" or "No
  // specific workspace", which it would only refuse.
  const calls = await openConsole(page);
  await openUsers(page);
  await page.getByRole("button", { name: "Invite user" }).click();
  await page.getByLabel("Email").fill("jane@new.test");
  await page.getByLabel("First name").fill("Jane");
  await page.getByLabel("Last name").fill("Smith");

  const role = page.getByLabel("Initial Role");
  const workspace = page.getByLabel("Workspace");
  const create = page.getByRole("button", { name: "Create user" });
  await expect(role.locator("option", { hasText: "None" })).toHaveCount(0);
  await expect(role).toHaveValue("");
  // Required for assistive tech too, not only by the visual asterisk.
  await expect(role).toHaveJSProperty("required", true);
  // Asked for from the start, with the reason, before any role is picked.
  await expect(workspace).toBeVisible();
  await expect(workspace).toHaveJSProperty("required", true);
  await expect(page.getByText(WORKSPACE_HINT, { exact: true })).toBeVisible();
  await expect(create).toBeDisabled();

  // A builder role too is always placed in a workspace.
  await role.selectOption("developer");
  await expect(workspace.locator("option", { hasText: "No specific workspace" })).toHaveCount(0);
  await expect(workspace).toHaveValue("");
  await expect(create).toBeDisabled();

  await workspace.selectOption(WS_ID);
  await expect(create).toBeEnabled();
  await create.click();
  await expect(page.getByRole("status").filter({ hasText: "Invited jane@new.test." })).toBeVisible();
  const posts = calls.filter((r) => r.method() === "POST" && new URL(r.url()).pathname === "/api/admin/users");
  expect(posts.map((r) => r.postDataJSON())).toEqual([
    { email: "jane@new.test", first_name: "Jane", last_name: "Smith", role: "developer", workspace_id: WS_ID },
  ]);
});

test("a tenant admin's invite with a workspace still asks for a role", async ({ page }) => {
  // The workspace is offered before any role is picked, so it can be chosen
  // first; the form still holds the invite until a role is chosen too.
  const calls = await openConsole(page);
  await openUsers(page);
  await page.getByRole("button", { name: "Invite user" }).click();
  await page.getByLabel("Email").fill("jane@new.test");
  await page.getByLabel("First name").fill("Jane");
  await page.getByLabel("Last name").fill("Smith");

  const role = page.getByLabel("Initial Role");
  const workspace = page.getByLabel("Workspace");
  const create = page.getByRole("button", { name: "Create user" });
  await workspace.selectOption(WS_ID);
  await expect(role).toHaveValue("");
  await expect(create).toBeDisabled();

  await role.selectOption("business_user");
  await expect(workspace).toHaveValue(WS_ID);
  await expect(create).toBeEnabled();
  await create.click();
  await expect(page.getByRole("status").filter({ hasText: "Invited jane@new.test." })).toBeVisible();
  const posts = calls.filter((r) => r.method() === "POST" && new URL(r.url()).pathname === "/api/admin/users");
  expect(posts.map((r) => r.postDataJSON())).toEqual([
    { email: "jane@new.test", first_name: "Jane", last_name: "Smith", role: "business_user", workspace_id: WS_ID },
  ]);
});

test("a platform admin keeps inviting with no role or no specific workspace", async ({ page }) => {
  const calls = await openConsole(page, { persona: "platform_admin" });
  await openUsers(page);
  await page.getByRole("button", { name: "Invite user" }).click();
  await page.getByLabel("Email").fill("pat@new.test");
  await page.getByLabel("First name").fill("Pat");
  await page.getByLabel("Last name").fill("Smith");

  const role = page.getByLabel("Initial Role");
  await expect(role.locator("option", { hasText: "None" })).toHaveCount(1);
  await expect(role).toHaveValue("");
  await expect(role).toHaveJSProperty("required", false);
  await expect(page.getByText(WORKSPACE_HINT, { exact: true })).toHaveCount(0);
  const create = page.getByRole("button", { name: "Create user" });
  await expect(create).toBeEnabled();

  await role.selectOption("developer");
  const workspace = page.getByLabel("Workspace");
  await expect(workspace.locator("option", { hasText: "No specific workspace" })).toHaveCount(1);
  await expect(workspace).toHaveValue("");
  await expect(workspace).toHaveJSProperty("required", false);
  await expect(create).toBeEnabled();
  await create.click();
  await expect(page.getByRole("status").filter({ hasText: "Invited pat@new.test." })).toBeVisible();
  const post = calls.find((r) => r.method() === "POST" && new URL(r.url()).pathname === "/api/admin/users");
  expect(post?.postDataJSON()).toMatchObject({ email: "pat@new.test", role: "developer", workspace_id: "" });
});
