/**
 * Form records carry what the caller may do with them — "permissions", worked
 * out by the server with the same function that authorises PUT/DELETE
 * /api/records/{id} ("submitter + admins": the person who created a record may
 * edit or delete it and move it between draft and submitted while it is
 * undecided; a business admin of the workspace may do anything; everyone else
 * who reaches the form only reads). The UI offers exactly that and no more:
 * Run › Forms and the dashboard form widget both follow the server's answer,
 * and a record without an answer (an older server) gets no actions at all.
 *
 * The fixtures are the shapes a real server sends: permsFor mirrors
 * crudapp.RecordAccess.Permissions, whose set_status never holds the
 * record's current status. The one test that feeds other shapes says so.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const ALL_STATUSES = ["draft", "submitted", "approved", "rejected"];
const CREATOR_STATUSES = ["draft", "submitted"];

// Who the caller is to a record: an administrator of its application, its
// creator, or anyone else who reaches its form.
type Caller = "admin" | "creator" | "reader";

// internal/crudapp/permissions.go, RecordAccess.Permissions.
function permsFor(caller: Caller, status: string) {
  if (caller === "admin") {
    return { edit: true, delete: true, set_status: ALL_STATUSES.filter((s) => s !== status) };
  }
  if (caller === "creator" && CREATOR_STATUSES.includes(status)) {
    return { edit: true, delete: true, set_status: CREATOR_STATUSES.filter((s) => s !== status) };
  }
  return { edit: false, delete: false, set_status: [] as string[] };
}

type Rec = {
  id: string; vendor: string; status: string; createdBy: string | null;
  // Absent: an older server, which sends no "permissions".
  caller?: Caller;
  // Overrides permsFor: only for the test of shapes the server never sends.
  permissions?: object;
};

function toJSON(r: Rec) {
  return {
    id: r.id, form_id: "form-1", status: r.status, created_by: r.createdBy,
    data: { vendor: r.vendor, amount: 1000, category: "Software" },
    created_at: "2026-09-20T09:00:00Z", updated_at: "2026-09-20T09:00:00Z",
    ...(r.permissions ? { permissions: r.permissions } : r.caller ? { permissions: permsFor(r.caller, r.status) } : {}),
  };
}

type Sent = { method: string; path: string; body: Record<string, unknown> | null };
type Server = {
  records: Rec[];
  sent: Sent[];
  // Answers a PUT/DELETE instead of accepting it (a refusal, a conflict).
  refuse?: (s: Sent) => { status: number; error: string } | undefined;
};

// Serves form-1's records and accepts changes to them the way the gateway
// does: a PUT keeps what it leaves out, a DELETE removes the record, and the
// next list read shows the result.
async function serveRecords(page: Page, records: Rec[]): Promise<Server> {
  const server: Server = { records, sent: [] };
  // Registered after mockApi, so these answer first.
  await page.route("**/api/forms/*/records", (route) => route.fulfill({ json: server.records.map(toJSON) }));
  await page.route("**/api/records/*", (route) => {
    const req = route.request();
    const s: Sent = { method: req.method(), path: new URL(req.url()).pathname, body: req.postData() ? req.postDataJSON() : null };
    server.sent.push(s);
    const refusal = server.refuse?.(s);
    if (refusal) return route.fulfill({ status: refusal.status, json: { error: refusal.error } });
    const id = s.path.split("/").pop();
    if (s.method === "DELETE") server.records = server.records.filter((r) => r.id !== id);
    if (s.method === "PUT" && typeof s.body?.status === "string") {
      const r = server.records.find((x) => x.id === id);
      if (r) r.status = s.body.status;
    }
    return route.fulfill({ json: { status: "ok" } });
  });
  return server;
}

function setServerStatus(server: Server, id: string, status: string) {
  server.records.find((r) => r.id === id)!.status = status;
}

const row = (page: Page, vendor: string) => page.getByRole("row").filter({ hasText: vendor });

async function optionsOf(select: ReturnType<Page["getByLabel"]>) {
  return select.locator("option").evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value));
}

async function openRunForms(page: Page, firstVendor: string) {
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Forms" }).click();
  await page.getByRole("button", { name: "Purchase Request" }).click();
  await expect(row(page, firstVendor)).toBeVisible();
}

async function openFormDashboard(page: Page, firstVendor: string) {
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "OPEX form 2" }).first().click();
  await expect(row(page, firstVendor)).toBeVisible();
}

async function deleteFromRow(page: Page, vendor: string) {
  await row(page, vendor).getByRole("button", { name: "Delete record" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Delete" }).click();
}

const editor = (page: Page) => page.locator(".mvx-panel", { hasText: "Edit Record" });

// What a business user of the workspace (u-1) is told.
function asSubmitter(): Rec[] {
  return [
    { id: "rec-own", vendor: "Own Vendor", status: "submitted", createdBy: "u-1", caller: "creator" },
    { id: "rec-other", vendor: "Other Vendor", status: "submitted", createdBy: "u-2", caller: "reader" },
    { id: "rec-decided", vendor: "Decided Vendor", status: "approved", createdBy: "u-1", caller: "creator" },
    { id: "rec-legacy", vendor: "Legacy Vendor", status: "draft", createdBy: null }, // an older server
  ];
}

// What a business admin of the workspace is told about records like those.
function asAdmin(): Rec[] {
  return [
    { id: "rec-own", vendor: "Own Vendor", status: "submitted", createdBy: "u-1", caller: "admin" },
    { id: "rec-other", vendor: "Other Vendor", status: "submitted", createdBy: "u-2", caller: "admin" },
    { id: "rec-decided", vendor: "Decided Vendor", status: "approved", createdBy: "u-1", caller: "admin" },
    { id: "rec-rejected", vendor: "Rejected Vendor", status: "rejected", createdBy: "u-2", caller: "admin" },
  ];
}

test("recordStatusOptions: the current status and the allowed ones, in lifecycle order", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "dept_head");
  const perms = (set_status: string[]) => ({ edit: true, delete: true, set_status });
  const cases = [
    // What the server sends.
    { status: "submitted", permissions: permsFor("creator", "submitted") },
    { status: "draft", permissions: permsFor("creator", "draft") },
    { status: "rejected", permissions: permsFor("admin", "rejected") },
    { status: "approved", permissions: permsFor("admin", "approved") },
    { status: "submitted", permissions: permsFor("reader", "submitted") },
    { status: "approved", permissions: permsFor("creator", "approved") },
    { status: "draft" }, // an older server: no permissions
    // Defensive: the current status among the allowed ones, and a status the
    // client does not know (it goes last).
    { status: "submitted", permissions: perms(["submitted"]) },
    { status: "archived", permissions: perms(["draft"]) },
  ].map((c) => ({ id: "r", form_id: "f", data: {}, created_by: null, created_at: "", updated_at: "", ...c }));

  const got = await page.evaluate(async (recs) => {
    const mod = await import("/src/api/client.ts");
    return recs.map((r) => mod.recordStatusOptions(r));
  }, cases);

  expect(got).toEqual([
    { options: ["draft", "submitted"], changeable: true },
    { options: ["draft", "submitted"], changeable: true },
    { options: ALL_STATUSES, changeable: true },
    { options: ALL_STATUSES, changeable: true },
    { options: [], changeable: false },
    { options: [], changeable: false },
    { options: [], changeable: false },
    { options: ["submitted"], changeable: false },
    { options: ["draft", "archived"], changeable: true },
  ]);
});

test("dashboard form widget: a business user may move their own undecided record between draft and submitted, and nothing else", async ({ page }) => {
  await mockApi(page);
  const server = await serveRecords(page, asSubmitter());
  await loadAs(page, "dept_head");
  await openFormDashboard(page, "Own Vendor");

  const own = row(page, "Own Vendor");
  const ownStatus = own.getByLabel("Record status");
  expect(await optionsOf(ownStatus)).toEqual(["draft", "submitted"]);
  await expect(ownStatus).toHaveValue("submitted");
  await expect(own.getByRole("button", { name: "Delete record" })).toBeVisible();

  // Another user's record, one already decided, and one the server said
  // nothing about: read-only — the status shows, nothing changes it.
  for (const vendor of ["Other Vendor", "Decided Vendor", "Legacy Vendor"]) {
    const r = row(page, vendor);
    await expect(r.getByLabel("Record status")).toHaveCount(0);
    await expect(r.getByRole("button", { name: "Delete record" })).toHaveCount(0);
  }
  await expect(row(page, "Other Vendor").getByText("submitted")).toBeVisible();
  await expect(row(page, "Decided Vendor").getByText("approved")).toBeVisible();

  // A status change sends the status alone: resending the fields as this
  // list last saw them would undo anyone's edit made since.
  await ownStatus.selectOption("draft");
  await expect.poll(() => server.sent.length).toBe(1);
  expect(server.sent[0]).toEqual({ method: "PUT", path: "/api/records/rec-own", body: { status: "draft" } });
  await expect(ownStatus).toHaveValue("draft");
  expect(await optionsOf(ownStatus)).toEqual(["draft", "submitted"]);
});

test("dashboard form widget: a status change refreshes the figures beside it", async ({ page }) => {
  await mockApi(page);
  await serveRecords(page, asSubmitter());
  // The form dashboard, with a grid next to the form.
  await page.route("**/api/dashboards/dash-3", (route) => route.fulfill({
    json: {
      id: "dash-3", name: "OPEX form 2", tags: ["finance"], widgets: [
        { id: "w3", widget_type: "form", ref_id: "form-1", content: null, sort_order: 0, col_start: 1, col_span: 12 },
        { id: "w3g", widget_type: "grid", ref_id: "grid-1", content: null, sort_order: 1, col_start: 1, col_span: 12 },
      ],
    },
  }));
  let metaReads = 0;
  page.on("request", (req) => {
    const u = new URL(req.url());
    if (u.pathname === "/api/grid" && u.searchParams.get("meta_only") === "1") metaReads++;
  });
  await loadAs(page, "dept_head");
  await openFormDashboard(page, "Own Vendor");
  await expect.poll(() => metaReads).toBeGreaterThan(0);
  const before = metaReads;

  await row(page, "Own Vendor").getByLabel("Record status").selectOption("draft");
  await expect.poll(() => metaReads, { timeout: 5000 }).toBeGreaterThan(before);
});

test("dashboard form widget: the error shown is the latest failed action's", async ({ page }) => {
  await mockApi(page);
  const records: Rec[] = [
    { id: "rec-own", vendor: "Own Vendor", status: "submitted", createdBy: "u-1", caller: "creator" },
    { id: "rec-own-2", vendor: "Northwind Supplies", status: "draft", createdBy: "u-1", caller: "creator" },
  ];
  const server = await serveRecords(page, records);
  server.refuse = (s) => s.method === "PUT"
    ? { status: 403, error: "forbidden: you may not move this record from submitted to draft" }
    : { status: 409, error: "the record changed since it was read; reload it and try again" };
  await loadAs(page, "dept_head");
  await openFormDashboard(page, "Own Vendor");

  await row(page, "Own Vendor").getByLabel("Record status").selectOption("draft");
  const alert = page.locator(".mvx-inline-alert--danger");
  await expect(alert).toContainText("403: forbidden: you may not move this record from submitted to draft");

  // A later failure replaces the earlier one's message.
  await deleteFromRow(page, "Northwind Supplies");
  await expect(alert).toContainText("409: the record changed since it was read");
  await expect(alert).not.toContainText("403");
  await expect(alert).toHaveText("Record not deleted — 409: the record changed since it was read; reload it and try again");
});

test("Run › Forms: the edit pencil, delete and statuses follow the record's permissions", async ({ page }) => {
  await mockApi(page);
  // The Forms tab is in the business-admin group; what the server says about
  // each record is what counts (here: the caller is only a business user of
  // the application these records belong to).
  const server = await serveRecords(page, asSubmitter());
  await loadAs(page, "finance");
  await openRunForms(page, "Own Vendor");

  for (const vendor of ["Other Vendor", "Decided Vendor", "Legacy Vendor"]) {
    const r = row(page, vendor);
    await expect(r.getByRole("button", { name: "Edit record" })).toHaveCount(0);
    await expect(r.getByRole("button", { name: "Delete record" })).toHaveCount(0);
  }

  const own = row(page, "Own Vendor");
  await expect(own.getByRole("button", { name: "Delete record" })).toBeVisible();

  // Fields alone: the status is not sent, so the server keeps its own.
  await own.getByRole("button", { name: "Edit record" }).click();
  const status = page.getByLabel("Status", { exact: true });
  await expect(status).toBeEnabled();
  await expect(status).toHaveValue("submitted");
  expect(await optionsOf(status)).toEqual(["draft", "submitted"]);
  await editor(page).getByRole("textbox").first().fill("Own Vendor Ltd");
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => server.sent.length).toBe(1);
  expect(server.sent[0]).toMatchObject({ method: "PUT", path: "/api/records/rec-own", body: { data: { vendor: "Own Vendor Ltd" } } });
  expect(server.sent[0].body).not.toHaveProperty("status");
  await expect(editor(page)).toHaveCount(0);

  // A status the user picks is sent.
  await own.getByRole("button", { name: "Edit record" }).click();
  await status.selectOption("draft");
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => server.sent.length).toBe(2);
  expect(server.sent[1]).toMatchObject({ method: "PUT", path: "/api/records/rec-own", body: { status: "draft" } });
});

test("Run › Forms: a refusal from the server is shown in the editor", async ({ page }) => {
  await mockApi(page);
  const server = await serveRecords(page, asSubmitter());
  server.refuse = () => ({ status: 403, error: "forbidden: you may not change this record" });
  await loadAs(page, "finance");
  await openRunForms(page, "Own Vendor");

  await row(page, "Own Vendor").getByRole("button", { name: "Edit record" }).click();
  await page.getByRole("button", { name: "Save" }).click();
  await expect(editor(page).getByText("403: forbidden: you may not change this record")).toBeVisible();
});

test("a business admin may edit, set any status on and delete every record", async ({ page }) => {
  await mockApi(page);
  const server = await serveRecords(page, asAdmin());
  await loadAs(page, "finance");
  await openRunForms(page, "Own Vendor");

  for (const vendor of ["Own Vendor", "Other Vendor", "Decided Vendor", "Rejected Vendor"]) {
    const r = row(page, vendor);
    await expect(r.getByRole("button", { name: "Edit record" })).toBeVisible();
    await expect(r.getByRole("button", { name: "Delete record" })).toBeVisible();
  }

  // Every status, in lifecycle order, whatever the record's status.
  const status = page.getByLabel("Status", { exact: true });
  await row(page, "Rejected Vendor").getByRole("button", { name: "Edit record" }).click();
  await expect(status).toHaveValue("rejected");
  expect(await optionsOf(status)).toEqual(ALL_STATUSES);

  await row(page, "Other Vendor").getByRole("button", { name: "Edit record" }).click();
  await expect(status).toHaveValue("submitted");
  expect(await optionsOf(status)).toEqual(ALL_STATUSES);
  await status.selectOption("approved");
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => server.sent.length).toBe(1);
  expect(server.sent[0]).toMatchObject({ method: "PUT", path: "/api/records/rec-other", body: { status: "approved" } });

  await deleteFromRow(page, "Decided Vendor");
  await expect.poll(() => server.sent.length).toBe(2);
  expect(server.sent[1]).toMatchObject({ method: "DELETE", path: "/api/records/rec-decided" });

  // The dashboard form widget offers the same.
  await openFormDashboard(page, "Own Vendor");
  const rejected = row(page, "Rejected Vendor").getByLabel("Record status");
  await expect(rejected).toHaveValue("rejected");
  expect(await optionsOf(rejected)).toEqual(ALL_STATUSES);
  await expect(row(page, "Other Vendor").getByRole("button", { name: "Delete record" })).toBeVisible();
});

test("Run › Forms: a save does not undo a decision made while the editor was open", async ({ page }) => {
  await mockApi(page);
  const server = await serveRecords(page, asAdmin());
  await loadAs(page, "finance");
  await openRunForms(page, "Own Vendor");

  await row(page, "Other Vendor").getByRole("button", { name: "Edit record" }).click();
  const status = page.getByLabel("Status", { exact: true });
  await expect(status).toHaveValue("submitted");

  // Another admin approves it; this list refreshes (here: after a delete).
  setServerStatus(server, "rec-other", "approved");
  await deleteFromRow(page, "Decided Vendor");
  await expect(row(page, "Other Vendor").getByText("approved")).toBeVisible();

  await expect(editor(page).getByText("This record was moved to approved while you were editing it.")).toBeVisible();
  await expect(status).toHaveValue("approved");
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => server.sent.length).toBe(2);
  expect(server.sent[1]).toMatchObject({ method: "PUT", path: "/api/records/rec-other" });
  expect(server.sent[1].body).not.toHaveProperty("status");
});

test("Run › Forms: losing the right to edit closes the editor with a notice, and it does not come back", async ({ page }) => {
  await mockApi(page);
  const server = await serveRecords(page, [
    { id: "rec-own", vendor: "Own Vendor", status: "submitted", createdBy: "u-1", caller: "creator" },
    { id: "rec-own-2", vendor: "Northwind Supplies", status: "draft", createdBy: "u-1", caller: "creator" },
    { id: "rec-own-3", vendor: "Contoso Parts", status: "draft", createdBy: "u-1", caller: "creator" },
  ]);
  await loadAs(page, "finance");
  await openRunForms(page, "Own Vendor");

  await row(page, "Own Vendor").getByRole("button", { name: "Edit record" }).click();
  await editor(page).getByRole("textbox").first().fill("Unsaved change");

  // An admin approves it; the list refreshes.
  setServerStatus(server, "rec-own", "approved");
  await deleteFromRow(page, "Northwind Supplies");
  await expect(row(page, "Northwind Supplies")).toHaveCount(0);
  await expect(editor(page)).toHaveCount(0);
  await expect(page.getByText("This record can no longer be edited — it is now approved. Your changes were not saved.")).toBeVisible();

  // Re-opened by an admin, it is editable again — but the old editor and
  // its draft do not reappear on their own.
  setServerStatus(server, "rec-own", "submitted");
  await deleteFromRow(page, "Contoso Parts");
  await expect(row(page, "Contoso Parts")).toHaveCount(0);
  await expect(row(page, "Own Vendor").getByRole("button", { name: "Edit record" })).toBeVisible();
  await expect(editor(page)).toHaveCount(0);
  expect(server.sent.filter((s) => s.method === "PUT")).toHaveLength(0);
});

// Not shapes the server sends (with edit, set_status is never empty and never
// holds the current status); kept so an unexpected answer degrades safely.
test("Run › Forms edit dialog, defensive: status read-only when only the current one is allowed, hidden when none is", async ({ page }) => {
  await mockApi(page);
  await serveRecords(page, [
    { id: "rec-fixed", vendor: "Fixed Status Vendor", status: "submitted", createdBy: "u-1", permissions: { edit: true, delete: false, set_status: ["submitted"] } },
    { id: "rec-fields", vendor: "Fields Only Vendor", status: "draft", createdBy: "u-1", permissions: { edit: true, delete: false, set_status: [] } },
  ]);
  await loadAs(page, "finance");
  await openRunForms(page, "Fixed Status Vendor");

  await row(page, "Fixed Status Vendor").getByRole("button", { name: "Edit record" }).click();
  const status = page.getByLabel("Status", { exact: true });
  await expect(status).toBeDisabled();
  await expect(status).toHaveValue("submitted");
  await expect(row(page, "Fixed Status Vendor").getByRole("button", { name: "Delete record" })).toHaveCount(0);

  await row(page, "Fields Only Vendor").getByRole("button", { name: "Edit record" }).click();
  await expect(editor(page).getByRole("textbox").first()).toHaveValue("Fields Only Vendor");
  await expect(page.getByLabel("Status", { exact: true })).toHaveCount(0);
});
