/**
 * A dimension field holding several members ("multiple": true, 2026-11-25):
 * a deal's contacts. A card and a list show every contact named; the deal's
 * form picks several, each a chip taken off with its ×, and saves them as a
 * list of codes; the list's column for them offers no sort (a list has no
 * order); the record's page opens each contact.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const CONTACTS = {
  id: "contacts", model_id: "model-1", name: "contacts", label: "Contacts", edit_access: "everyone",
  keeps_dimension_id: "dim-con", member_label_field: "full_name",
  fields: [
    { name: "code", label: "Code", type: "autocode", required: false },
    { name: "full_name", label: "Full Name", type: "text", required: true },
  ],
  created_at: "2026-10-01T09:00:00Z",
  permissions: { sync: false, create: true, create_statuses: [] },
};
const DEALS = {
  id: "deals", model_id: "model-1", name: "deals", label: "Deals", edit_access: "everyone",
  fields: [
    { name: "deal_name", label: "Deal Name", type: "text", required: true },
    { name: "stage", label: "Deal Stage", type: "dimension", dimension_id: "dim-stage", required: true },
    { name: "contacts", label: "Contacts", type: "dimension", dimension_id: "dim-con", required: false, multiple: true, on_delete: "clear" },
  ],
  created_at: "2026-10-01T09:00:00Z",
  permissions: { sync: false, create: true, create_statuses: [] },
};
const perms = { edit: true, delete: true, set_status: [] };
const DIMENSIONS = [
  { id: "dim-stage", name: "Deal Stage", agg_rule: "sum", members: [{ id: "s1", code: "QUAL", label: "Qualification" }, { id: "s2", code: "MEET", label: "1st Meeting" }] },
  { id: "dim-con", name: "Contact", agg_rule: "sum", members: [
    { id: "c1", code: "CON-1", label: "Ann Lee" }, { id: "c2", code: "CON-2", label: "Bob Stone" }, { id: "c3", code: "CON-3", label: "Cid Moss" }] },
];
const records = [{
  id: "d-1", form_id: "deals", data: { deal_name: "Big", stage: "QUAL", contacts: ["CON-1", "CON-2"] },
  status: "", created_by: "u-1", created_at: "2026-10-01T09:00:00Z", updated_at: "2026-10-01T09:00:00Z", permissions: perms,
}];

async function serve(page: Page, widget: Record<string, unknown>) {
  const created: Record<string, unknown>[] = [];
  const dashboard = { id: "dash-1", name: "Deals", tags: [], widgets: [{ id: "w1", ref_id: "deals", content: null, title: "Deals",
    sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 1000, size_h: 520, ...widget }] };
  await page.route((url) => url.pathname === "/api/dashboards" || url.pathname.startsWith("/api/dashboards/"), (route) =>
    route.fulfill({ json: new URL(route.request().url()).pathname === "/api/dashboards" ? [dashboard] : dashboard }));
  await page.route((url) => url.pathname === "/api/forms", (route) => route.fulfill({ json: [CONTACTS, DEALS] }));
  await page.route((url) => url.pathname === "/api/dimensions", (route) => route.fulfill({ json: DIMENSIONS }));
  await page.route((url) => url.pathname === "/api/forms/deals/board-columns", (route) => route.fulfill({ json: { columns: [
    { key: "QUAL", label: "Qualification" }, { key: "MEET", label: "1st Meeting" }] } }));
  await page.route((url) => url.pathname === "/api/forms/deals/records/query", (route) => {
    const q = route.request().postDataJSON() as { group_by?: unknown[] };
    if (q.group_by) return route.fulfill({ json: { kind: "totals", table: { id: "deals" }, columns: [], rows: [{ stage: "QUAL", count: 1 }], total_groups: 1 } });
    return route.fulfill({ json: { kind: "records", table: { id: "deals" }, columns: [], records, total: records.length } });
  });
  await page.route((url) => url.pathname === "/api/forms/deals/records", (route) => {
    created.push(route.request().postDataJSON());
    return route.fulfill({ json: { ...records[0], id: "d-2", data: (route.request().postDataJSON() as { data: unknown }).data } });
  });
  await page.route((url) => url.pathname.startsWith("/api/records/"), (route) => {
    const u = route.request().url();
    if (u.includes("/history")) return route.fulfill({ json: { history: [] } });
    if (u.includes("/related")) return route.fulfill({ json: { related: [] } });
    return route.fulfill({ json: records[0] });
  });
  return created;
}

async function open(page: Page) {
  await loadAs(page, "dept_head");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
}

test.describe("a field holding several members", () => {
  test.beforeEach(async ({ page }) => { await mockApi(page); });

  test("a card shows every contact; the form picks several as chips and saves a list", async ({ page }) => {
    const created = await serve(page, { widget_type: "board", widget_props: {
      board: { column_field: "stage", card_fields: ["contacts"] }, new_record_button: true } });
    await open(page);
    await expect(page.getByRole("article", { name: "Big" })).toContainText("Contacts: Ann Lee, Bob Stone");

    await page.getByRole("button", { name: "New deal" }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Deal Name").fill("Small");
    const add = dlg.getByLabel("Add to Contacts");
    for (const who of ["Cid Moss", "Ann Lee", "Bob Stone"]) {
      await add.click();
      await page.getByRole("listbox").getByRole("option", { name: who }).click();
    }
    const contacts = dlg.getByRole("group", { name: "Contacts" });
    await expect(contacts.getByRole("button", { name: /^Remove / })).toHaveCount(3);
    // A chosen contact is offered no more.
    await add.click();
    await expect(page.getByRole("listbox").getByRole("option")).toHaveCount(0);
    // Escape closes the list, not the form.
    await page.keyboard.press("Escape");
    await expect(page.getByRole("listbox")).toHaveCount(0);
    await expect(dlg).toBeVisible();
    await contacts.getByRole("button", { name: "Remove Ann Lee" }).click();
    await expect(contacts.getByRole("button", { name: /^Remove / })).toHaveCount(2);
    await dlg.getByRole("button", { name: "Save", exact: true }).click();
    await expect.poll(() => created.length).toBe(1);
    expect(created[0]).toMatchObject({ data: { deal_name: "Small", contacts: ["CON-3", "CON-2"] } });
  });

  test("a list shows the contacts with no sort on them; the record's page opens each", async ({ page }) => {
    await serve(page, { widget_type: "form", widget_props: {} });
    await open(page);
    const row = page.getByRole("row", { name: /Big/ });
    await expect(row).toContainText("Ann Lee, Bob Stone");
    await expect(page.getByRole("columnheader", { name: "Contacts" }).getByRole("button")).toHaveCount(0);
    await expect(page.getByRole("columnheader", { name: "Deal Name" }).getByRole("button")).toHaveCount(1);
    await row.click();
    const dlg = page.getByRole("dialog");
    await expect(dlg.getByRole("button", { name: "Ann Lee" })).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Bob Stone" })).toBeVisible();
  });
});
