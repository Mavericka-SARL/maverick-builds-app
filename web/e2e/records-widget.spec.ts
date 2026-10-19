/**
 * The records widget and the developer's Records tab, against a stubbed
 * server that records what the console sends:
 *
 *  - the widget's list is the server's records query: search, filters and a
 *    header sort are sent to it (so they cover every record, not one page);
 *  - "+ New deal" (the table's record) shows only when the widget's developer added it;
 *  - a table without a status field shows no status column and creates
 *    records without a status;
 *  - a member the person may only read is not offered where the save posts
 *    it into a metric;
 *  - an auto-code field is the server's: shown, never typed;
 *  - the Records tab saves Auto code and Status fields and the
 *    "own records only" setting.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

type Body = Record<string, unknown>;

const DIMS = [{
  id: "dim-region", name: "Region", members: [
    { id: "m-eu", code: "EU", label: "Europe", parent_member_id: null, access: "read" },
    { id: "m-us", code: "US", label: "United States", parent_member_id: null },
  ],
}];

function table(over: Body = {}) {
  return {
    id: "form-1", model_id: "model-1", name: "deal", label: "Deals", created_at: "2026-05-01T09:00:00Z",
    fields: [
      { name: "code", label: "Code", type: "autocode", required: false, code_prefix: "D-", code_digits: 3 },
      { name: "region", label: "Region", type: "dimension", required: true, dimension_id: "dim-region" },
      { name: "amount", label: "Amount", type: "number", required: true },
    ],
    permissions: { sync: false, create: true, create_statuses: [] },
    posting_fields: { region: [""] },
    ...over,
  };
}

const RECORDS = [
  { id: "r1", form_id: "form-1", data: { code: "D-001", region: "US", amount: 120 }, status: "", created_by: "u-1",
    created_at: "2026-09-20T09:00:00Z", updated_at: "2026-09-20T09:00:00Z", permissions: { edit: true, delete: true, set_status: [] } },
  { id: "r2", form_id: "form-1", data: { code: "D-002", region: "EU", amount: 40 }, status: "", created_by: "u-2",
    created_at: "2026-09-21T09:00:00Z", updated_at: "2026-09-21T09:00:00Z", permissions: { edit: false, delete: false, set_status: [] } },
];

type Server = { queries: Body[]; posts: Body[] };

async function serve(page: Page, form: Body, widgetProps: Body | undefined, widget: Body = {}): Promise<Server> {
  const server: Server = { queries: [], posts: [] };
  await page.route((url) => url.pathname === "/api/forms", (route) => route.fulfill({ json: [form] }));
  await page.route((url) => url.pathname === "/api/dimensions", (route) => route.fulfill({ json: DIMS }));
  await page.route((url) => url.pathname === "/api/forms/form-1/records/query", (route) => {
    server.queries.push(route.request().postDataJSON());
    return route.fulfill({ json: { kind: "records", table: { id: "form-1", name: "deal", label: "Deals" }, columns: [], records: RECORDS, total: 2 } });
  });
  await page.route((url) => url.pathname === "/api/forms/form-1/records", (route) => {
    server.posts.push(route.request().postDataJSON());
    return route.fulfill({ json: { ...RECORDS[0], id: "r3" } });
  });
  const dashboard = { id: "dash-3", name: "OPEX form 2", tags: ["finance"], folder_id: null,
    widgets: [{ id: "w3", widget_type: "form", ref_id: "form-1", content: null, sort_order: 0, col_start: 1, col_span: 12, widget_props: widgetProps ?? null, ...widget }] };
  await page.route((url) => url.pathname === "/api/dashboards/dash-3", (route) => route.fulfill({ json: dashboard }));
  return server;
}

async function openWidget(page: Page) {
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "OPEX form 2" }).first().click();
  await expect(page.getByRole("row").filter({ hasText: "D-001" })).toBeVisible();
}

const lastQuery = (s: Server) => s.queries[s.queries.length - 1];
// The new-record panel's member picker (the list's header has a "Region" sort button too).
const newRecordRegion = (page: Page) => page.getByLabel("Region", { exact: true });

test("the list is the records query: search, filters and a header sort go to the server", async ({ page }) => {
  await mockApi(page);
  const server = await serve(page, table(), { new_record_button: true });
  await loadAs(page, "dept_head");
  await openWidget(page);

  // A member reads by its name, not its code; a table without a status field has no status column.
  await expect(page.getByRole("row").filter({ hasText: "D-001" }).getByText("United States")).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Status" })).toHaveCount(0);
  await expect(page.getByText("2 records")).toBeVisible();

  await page.getByRole("searchbox", { name: "Search Deals" }).fill("europe");
  await expect.poll(() => lastQuery(server)?.search).toBe("europe");

  await page.getByRole("columnheader", { name: "Amount" }).getByRole("button").click();
  await expect.poll(() => JSON.stringify(lastQuery(server)?.sort)).toBe(JSON.stringify([{ field: "amount", desc: false }]));
  await page.getByRole("columnheader", { name: "Amount" }).getByRole("button").click();
  await expect.poll(() => JSON.stringify(lastQuery(server)?.sort)).toBe(JSON.stringify([{ field: "amount", desc: true }]));
  await expect(page.getByRole("columnheader", { name: "Amount" })).toHaveAttribute("aria-sort", "descending");

  await page.getByRole("button", { name: "Filter" }).click();
  const builder = page.getByRole("group", { name: "New filter" });
  await builder.getByLabel("Field").selectOption("region");
  await builder.getByLabel("Member").selectOption("US");
  await builder.getByRole("button", { name: "Add filter" }).click();
  await expect.poll(() => JSON.stringify(lastQuery(server)?.filters)).toBe(JSON.stringify([{ field: "region", op: "eq", value: "US" }]));
  await expect(page.getByRole("button", { name: "Remove filter: Region is United States" })).toBeVisible();
  await page.getByRole("button", { name: "Remove filter: Region is United States" }).click();
  await expect(page.getByRole("button", { name: /Remove filter/ })).toHaveCount(0);
});

test("a titled list has one heading — the widget's title, saying which filters it keeps", async ({ page }) => {
  await mockApi(page);
  await serve(page, table(), { record_filters: [{ field: "region", op: "eq", value: "US" }] }, { title: "US deals", show_title: true });
  await loadAs(page, "dept_head");
  await openWidget(page);
  await expect(page.locator(".mvx-panel-head__title")).toHaveText("US deals");
  await expect(page.locator(".mvx-widget__header")).toHaveCount(0);
  await expect(page.getByLabel("Filter set on this widget: Region is United States")).toBeVisible();
});

test("+ New deal shows only when the widget's developer added it", async ({ page }) => {
  await mockApi(page);
  await serve(page, table(), undefined);
  await loadAs(page, "dept_head");
  await openWidget(page);
  await expect(page.getByRole("button", { name: "New deal" })).toHaveCount(0);
  // Import, under the list's "⋯", still adds records for a person who may create them.
  await page.getByRole("button", { name: "Deals actions" }).click();
  await expect(page.getByRole("menuitem", { name: "Import" })).toBeVisible();
});

test("a new record: no status in a table without one, the code is the server's, a read-only member is not offered where it posts", async ({ page }) => {
  await mockApi(page);
  const server = await serve(page, table(), { new_record_button: true });
  await loadAs(page, "dept_head");
  await openWidget(page);
  await page.getByRole("button", { name: "New deal" }).click();

  await expect(page.getByLabel("Code")).toHaveValue("Given when the record is saved");
  await expect(page.getByLabel("Code")).toBeDisabled();
  await expect(page.getByLabel("Status")).toHaveCount(0);

  // Europe is read-only for this person and the save posts the region: not offered.
  await newRecordRegion(page).click();
  await expect(page.getByRole("option", { name: "United States" })).toBeVisible();
  await expect(page.getByRole("option", { name: "Europe" })).toHaveCount(0);
  await page.getByRole("option", { name: "United States" }).click();
  await page.getByLabel("Amount").fill("75");
  await page.getByRole("button", { name: "Save" }).click();
  await expect.poll(() => server.posts.length).toBe(1);
  expect(server.posts[0]).toEqual({ data: { region: "US", amount: 75 }, revision_id: "rev-1" });
});

test("a read-only member is offered where the save posts nothing", async ({ page }) => {
  await mockApi(page);
  await serve(page, table({ posting_fields: {} }), { new_record_button: true });
  await loadAs(page, "dept_head");
  await openWidget(page);
  await page.getByRole("button", { name: "New deal" }).click();
  await newRecordRegion(page).click();
  await expect(page.getByRole("option", { name: "Europe" })).toBeVisible();
});

test("the Records tab saves Auto code and Status fields and own-records-only", async ({ page }) => {
  await mockApi(page);
  const created: Body[] = [];
  await page.route((url) => url.pathname === "/api/forms", (route) => {
    if (route.request().method() === "POST") {
      created.push(route.request().postDataJSON());
      return route.fulfill({ json: { ...table(), id: "form-2" } });
    }
    return route.fulfill({ json: [] });
  });
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Records" }).first().click();
  await page.getByRole("button", { name: "New table" }).click();
  await page.getByLabel("Table name").fill("expense");
  await page.getByLabel("Display label").fill("Expenses");
  await page.getByLabel("Field name").fill("code");
  await page.getByLabel("Field label").fill("Code");
  await page.getByLabel("Field type").selectOption("autocode");
  await page.getByLabel("Code prefix").fill("EXP-");
  await page.getByRole("button", { name: "Add field" }).click();
  await page.getByLabel("Field type").nth(1).selectOption("status");
  // One status field per table.
  await page.getByRole("button", { name: "Add field" }).click();
  await expect(page.getByLabel("Field type").nth(2).locator("option[value=status]")).toBeDisabled();
  await page.getByRole("button", { name: "Remove field 3" }).click();
  await page.getByLabel("Each person sees only their own records").check();
  await page.getByRole("button", { name: "Create table" }).click();
  await expect.poll(() => created.length).toBe(1);
  expect(created[0]).toEqual({
    name: "expense", label: "Expenses", own_records_only: true,
    fields: [
      { name: "code", label: "Code", type: "autocode", required: false, code_prefix: "EXP-", code_digits: 4 },
      { name: "status", label: "Status", type: "status", required: false },
    ],
  });
});
