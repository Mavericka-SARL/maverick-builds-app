/**
 * What a caller may do with a form's records as a whole — "permissions" on
 * each form GET /api/forms lists: sync (POST /api/forms/{id}/sync, an
 * administrator's) and create_statuses (the statuses a new record may take:
 * draft and submitted for anyone who reaches the form, every status for an
 * administrator). The dashboard form widget — the one place business users
 * and business admins work on records; there is no Forms screen — offers
 * exactly that: "Sync to grid" only with sync, a status choice on a new record
 * only among create_statuses and only when there is more than one, and nothing
 * at all from an older server that sends no permissions; Import, which creates
 * records too, only with create_statuses. A sync the server refuses anyway
 * (403) says why in plain words, as a failure, and reloads the forms list so
 * "Sync to grid" follows the server's current answer.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const ALL_STATUSES = ["draft", "submitted", "approved", "rejected"];

type FormPerms = { sync: boolean; create_statuses: string[] };
// What the server tells a business user of the workspace, and an administrator.
const BUSINESS_USER: FormPerms = { sync: false, create_statuses: ["draft", "submitted"] };
const ADMIN: FormPerms = { sync: true, create_statuses: ALL_STATUSES };

const FORM = {
  id: "form-1", model_id: "model-1", name: "purchase_request", label: "Purchase Request",
  fields: [
    { name: "vendor", label: "Vendor", type: "text", required: true },
    { name: "amount", label: "Amount", type: "number", required: true },
  ],
  created_at: "2026-05-01T09:00:00Z",
};

type Server = { posts: Record<string, unknown>[]; syncs: number; perms: FormPerms | undefined };

// Serves form-1 with server.perms (none: an older server) and one record,
// accepts a new record, and answers a sync with syncStatus.
async function serveForm(page: Page, perms: FormPerms | undefined, syncStatus = 200): Promise<Server> {
  const server: Server = { posts: [], syncs: 0, perms };
  // Registered after mockApi, so these answer first.
  await page.route((url) => url.pathname === "/api/forms", (route) =>
    route.fulfill({ json: [{ ...FORM, ...(server.perms ? { permissions: server.perms } : {}) }] }));
  await page.route((url) => url.pathname === "/api/forms/form-1/records", (route) => {
    const req = route.request();
    if (req.method() === "POST") {
      server.posts.push(req.postDataJSON());
      return route.fulfill({ json: { id: "rec-new", form_id: "form-1", data: {}, status: "draft", created_by: "u-1", created_at: "", updated_at: "" } });
    }
    return route.fulfill({
      json: [{
        id: "rec-1", form_id: "form-1", data: { vendor: "Acme Cloud", amount: 100 }, status: "submitted", created_by: "u-1",
        created_at: "2026-09-20T09:00:00Z", updated_at: "2026-09-20T09:00:00Z",
        permissions: { edit: false, delete: false, set_status: [] },
      }],
    });
  });
  await page.route((url) => url.pathname === "/api/forms/form-1/sync", (route) => {
    server.syncs++;
    if (syncStatus === 403) {
      return route.fulfill({ status: 403, json: { error: "forbidden: only an administrator of this application syncs a form's records" } });
    }
    return route.fulfill({ json: { status: "ok", mappings: 1, records_processed: 1 } });
  });
  return server;
}

const row = (page: Page, text: string) => page.getByRole("row").filter({ hasText: text });

// The form widget on a dashboard, as a business user (dept_head) and as a
// business admin (finance) open it. What it offers follows the server's
// permissions, not the persona: each test serves the permissions it means.
const openFormDashboard = (persona: string) => async (page: Page) => {
  await loadAs(page, persona);
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "OPEX form 2" }).first().click();
  await expect(row(page, "Acme Cloud")).toBeVisible();
};

const surfaces = [
  { name: "form widget (business user)", open: openFormDashboard("dept_head") },
  { name: "form widget (business admin)", open: openFormDashboard("finance") },
];

async function optionsOf(select: ReturnType<Page["getByLabel"]>) {
  return select.locator("option").evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value));
}

// A sync's outcome as the alert shows it, tone included.
const alert = (page: Page, tone: "success" | "danger", text: string | RegExp) =>
  page.locator(`.mvx-inline-alert--${tone}`).filter({ hasText: text });

async function fillNewRecord(page: Page) {
  await page.getByRole("button", { name: "New record" }).click();
  // By its label: Field ties the label to the control FormFieldInput renders.
  await page.getByLabel("Vendor").fill("Beta Soft");
  await expect(page.getByLabel("Amount")).toHaveAttribute("type", "number");
}

for (const s of surfaces) {
  test(`${s.name}: a business user gets no Sync, and creates a draft or a submission`, async ({ page }) => {
    await mockApi(page);
    const server = await serveForm(page, BUSINESS_USER);
    await s.open(page);

    await expect(page.getByRole("button", { name: "Sync to grid" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Import" })).toHaveCount(1);
    await expect(page.getByRole("button", { name: "Export" })).toHaveCount(1);

    await fillNewRecord(page);
    const status = page.getByLabel("Status");
    expect(await optionsOf(status)).toEqual(["draft", "submitted"]);
    await expect(status).toHaveValue("draft");
    await status.selectOption("submitted");
    await page.getByRole("button", { name: "Save" }).click();
    await expect.poll(() => server.posts.length).toBe(1);
    expect(server.posts[0]).toMatchObject({ status: "submitted", data: { vendor: "Beta Soft" } });
  });

  test(`${s.name}: an administrator syncs, and creates a record in any status`, async ({ page }) => {
    await mockApi(page);
    const server = await serveForm(page, ADMIN);
    await s.open(page);

    await page.getByRole("button", { name: "Sync to grid" }).click();
    await expect.poll(() => server.syncs).toBe(1);
    await expect(alert(page, "success", /Synced — 1 record\(s\)/)).toBeVisible();

    await fillNewRecord(page);
    const status = page.getByLabel("Status");
    expect(await optionsOf(status)).toEqual(ALL_STATUSES);
    // Left alone, the new record is a draft.
    await page.getByRole("button", { name: "Save" }).click();
    await expect.poll(() => server.posts.length).toBe(1);
    expect(server.posts[0]).toMatchObject({ status: "draft" });
  });

  test(`${s.name}: a single create status is not offered as a choice`, async ({ page }) => {
    await mockApi(page);
    const server = await serveForm(page, { sync: false, create_statuses: ["draft"] });
    await s.open(page);

    await fillNewRecord(page);
    await expect(page.getByLabel("Status")).toHaveCount(0);
    await page.getByRole("button", { name: "Save" }).click();
    await expect.poll(() => server.posts.length).toBe(1);
    expect(server.posts[0]).toMatchObject({ status: "draft" });
  });

  test(`${s.name}: a sync the server refuses says only an administrator can sync, and drops Sync`, async ({ page }) => {
    await mockApi(page);
    // The list said sync, but the caller's role changed since it was read:
    // the server refuses, and now lists the form without sync.
    const server = await serveForm(page, ADMIN, 403);
    await s.open(page);
    server.perms = BUSINESS_USER;

    await page.getByRole("button", { name: "Sync to grid" }).click();
    await expect.poll(() => server.syncs).toBe(1);
    await expect(alert(page, "danger", "Only an administrator of this application can sync.")).toBeVisible();
    await expect(page.locator(".mvx-inline-alert--success")).toHaveCount(0);
    await expect(page.getByText(/Sync failed/)).toHaveCount(0);
    // The list is reloaded at once (not at the next 20 s poll), and the
    // button follows it.
    await expect(page.getByRole("button", { name: "Sync to grid" })).toHaveCount(0);
  });

  test(`${s.name}: an older server that sends no permissions gets neither Sync nor New record`, async ({ page }) => {
    await mockApi(page);
    await serveForm(page, undefined);
    await s.open(page);

    await expect(page.getByRole("button", { name: "Sync to grid" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "New record" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Import" })).toHaveCount(0);
  });
}
