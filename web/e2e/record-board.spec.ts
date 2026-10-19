/**
 * The board widget: a table's records as cards in columns — the members of
 * any dimension field (here a deal's stage) in their order — with each
 * column's count and total from the records query's totals, a card moved by
 * dragging it or by its "Move to" list (a PUT of the record with the field
 * changed), and no move into a column read-only for the viewer. A card opens
 * the record, whose field naming a member another table keeps (the account)
 * offers "New" — that table's form in place, the new record picked once saved.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const ACCOUNTS = {
  id: "accounts", model_id: "model-1", name: "accounts", label: "Accounts", edit_access: "everyone",
  keeps_dimension_id: "dim-acc", member_label_field: "company_name", member_root_label: "All accounts",
  fields: [
    { name: "code", label: "Code", type: "autocode", required: false },
    { name: "company_name", label: "Company Name", type: "text", required: true },
  ],
  created_at: "2026-10-01T09:00:00Z",
  permissions: { sync: false, create: true, create_statuses: [] },
};
const DEALS = {
  id: "deals", model_id: "model-1", name: "deals", label: "Deals", edit_access: "everyone",
  keeps_dimension_id: "dim-deal", member_label_field: "deal_name",
  fields: [
    { name: "code", label: "Code", type: "autocode", required: false },
    { name: "deal_name", label: "Deal Name", type: "text", required: true },
    { name: "account", label: "Account", type: "dimension", dimension_id: "dim-acc", required: true },
    { name: "stage", label: "Deal Stage", type: "dimension", dimension_id: "dim-stage", required: true, history: true },
    { name: "value", label: "Deal Value", type: "number", format: "money", required: false },
    { name: "next_date", label: "Next Action Date", type: "date", deadline: true, required: false },
  ],
  created_at: "2026-10-01T09:00:00Z",
  permissions: { sync: false, create: true, create_statuses: [] },
};
const perms = { edit: true, delete: true, set_status: [] };
const deal = (id: string, name: string, stage: string, value: number, next = "") => ({
  id, form_id: "deals", data: { code: id.toUpperCase(), deal_name: name, account: "ACC-1", stage, value, ...(next ? { next_date: next } : {}) },
  status: "", created_by: "u-1", created_at: "2026-10-01T09:00:00Z", updated_at: "2026-10-01T09:00:00Z", permissions: perms,
});
const DIMENSIONS = [
  { id: "dim-stage", name: "Deal Stage", agg_rule: "sum", members: [
    { id: "m1", code: "QUAL", label: "Qualification" }, { id: "m2", code: "MEET", label: "1st Meeting" }, { id: "m3", code: "WON", label: "Closed Won" }] },
  { id: "dim-acc", name: "Account", agg_rule: "sum", members: [{ id: "a1", code: "ACC-1", label: "Acme" }] },
];

type Server = { puts: { id: string; body: Record<string, unknown> }[]; created: Record<string, unknown>[] };

async function serveBoard(page: Page): Promise<Server> {
  const server: Server = { puts: [], created: [] };
  const records = [deal("d-1", "Big", "QUAL", 1000, "2020-01-01"), deal("d-2", "Small", "QUAL", 50), deal("d-3", "Done", "WON", 700)];
  const dashboard = { id: "dash-board", name: "Pipeline", tags: [], widgets: [{ id: "wb", widget_type: "board", ref_id: "deals", content: null, title: "Pipeline",
    sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 1000, size_h: 520,
    widget_props: { board: { column_field: "stage", sum_field: "value", card_fields: ["account", "value", "next_date"], color_property: "Color" }, new_record_button: true } }] };
  await page.route((url) => url.pathname === "/api/dashboards" || url.pathname.startsWith("/api/dashboards/"), (route) =>
    route.fulfill({ json: new URL(route.request().url()).pathname === "/api/dashboards" ? [dashboard] : dashboard }));
  await page.route((url) => url.pathname === "/api/forms", (route) => route.fulfill({ json: [ACCOUNTS, DEALS] }));
  await page.route((url) => url.pathname === "/api/dimensions", (route) => route.fulfill({ json: DIMENSIONS }));
  await page.route((url) => url.pathname === "/api/forms/deals/board-columns", (route) => route.fulfill({ json: { columns: [
    { key: "QUAL", label: "Qualification", color: "#4A90D9" }, { key: "MEET", label: "1st Meeting", color: "#56B4E9" },
    { key: "WON", label: "Closed Won", color: "#7ED3D9", read_only: true }] } }));
  await page.route((url) => url.pathname === "/api/forms/deals/records/query", (route) => {
    const q = route.request().postDataJSON() as { group_by?: unknown[] };
    if (q.group_by) {
      return route.fulfill({ json: { kind: "totals", table: { id: "deals" }, columns: [], rows: [
        { stage: "QUAL", count: 2, sum_value: 1050 }, { stage: "WON", count: 1, sum_value: 700 }], total_groups: 2 } });
    }
    return route.fulfill({ json: { kind: "records", table: { id: "deals" }, columns: [], records, total: records.length } });
  });
  await page.route((url) => url.pathname.startsWith("/api/records/"), (route) => {
    const req = route.request();
    const id = new URL(req.url()).pathname.split("/")[3];
    if (req.method() === "PUT") {
      server.puts.push({ id, body: req.postDataJSON() });
      return route.fulfill({ json: { status: "ok" } });
    }
    if (req.url().includes("/history")) return route.fulfill({ json: { history: [] } });
    if (req.url().includes("/related")) return route.fulfill({ json: { related: [] } });
    return route.fulfill({ json: records.find(r => r.id === id) ?? records[0] });
  });
  await page.route((url) => url.pathname === "/api/forms/accounts/records", (route) => {
    server.created.push(route.request().postDataJSON());
    return route.fulfill({ json: { id: "acc-new", form_id: "accounts", data: { code: "ACC-2", company_name: "Globex" }, status: "", created_by: "u-1",
      created_at: "", updated_at: "", permissions: perms } });
  });
  return server;
}

async function openBoard(page: Page) {
  await loadAs(page, "dept_head");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  // The one dashboard opens by itself.
  await expect(page.getByRole("listitem", { name: "Qualification" })).toBeVisible();
}

test.describe("board widget", () => {
  test.beforeEach(async ({ page }) => { await mockApi(page); });

  test("columns in the dimension's order, with counts and totals, cards with their fields", async ({ page }) => {
    await serveBoard(page);
    await openBoard(page);
    const cols = page.locator(".mvx-board__column");
    await expect(cols).toHaveCount(3);
    await expect(cols.nth(0)).toHaveAttribute("aria-label", "Qualification");
    await expect(cols.nth(2)).toHaveAttribute("aria-label", "Closed Won");
    const qual = page.getByRole("listitem", { name: "Qualification" });
    await expect(qual.locator(".mvx-board__column-count")).toHaveText("2");
    // The total reads whole; its title has it in full.
    await expect(qual.locator(".mvx-board__column-sum")).toHaveText("1,050");
    await expect(qual.locator(".mvx-board__column-sum")).toHaveAttribute("title", "Deal Value total: 1,050.00");
    await expect(qual.getByRole("article", { name: "Big" })).toContainText("Acme");
    // A next action whose date has passed reads as overdue.
    await expect(qual.getByRole("article", { name: "Big" }).locator(".mvx-overdue")).toContainText("overdue");
    // A column read-only for the viewer is marked, and its cards do not move.
    const won = page.getByRole("listitem", { name: "Closed Won" });
    await expect(won.getByLabel("Read-only for you")).toBeVisible();
  });

  test("a card moves by dragging it, and by its Move to list; nothing moves into a read-only column", async ({ page }) => {
    const server = await serveBoard(page);
    await openBoard(page);
    const big = page.getByRole("article", { name: "Big" });
    await big.dragTo(page.getByRole("listitem", { name: "1st Meeting" }));
    await expect.poll(() => server.puts.length).toBe(1);
    expect(server.puts[0]).toMatchObject({ id: "d-1", body: { data: { stage: "MEET", deal_name: "Big", account: "ACC-1" } } });

    const small = page.getByRole("article", { name: "Small" });
    await expect(small.getByLabel("Move Small to").locator("option", { hasText: "Closed Won" })).toBeDisabled();
    await small.getByLabel("Move Small to").selectOption("MEET");
    await expect.poll(() => server.puts.length).toBe(2);
    expect(server.puts[1]).toMatchObject({ id: "d-2", body: { data: { stage: "MEET" } } });
  });

  test("a card opens the record's page: details, history, Edit; a linked account opens in the same window", async ({ page }) => {
    await serveBoard(page);
    await page.route((url) => url.pathname === "/api/forms/accounts/records/query", (route) =>
      route.fulfill({ json: { kind: "records", table: { id: "accounts" }, columns: [], total: 1, records: [
        { id: "acc-1", form_id: "accounts", data: { code: "ACC-1", company_name: "Acme" }, status: "", created_by: "u-1", created_at: "", updated_at: "", permissions: perms }] } }));
    await openBoard(page);
    await page.getByRole("article", { name: "Big" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.locator("#dialog-title")).toHaveText("Big");
    await expect(dialog.getByRole("region", { name: "History" })).toBeVisible();
    await dialog.getByRole("button", { name: "Acme" }).click();
    await expect(dialog.locator("#dialog-title")).toHaveText("Acme");
    await dialog.getByRole("button", { name: /Back to Big/ }).click();
    await expect(dialog.locator("#dialog-title")).toHaveText("Big");
    await dialog.getByRole("button", { name: "Edit" }).click();
    await expect(dialog.getByLabel("Deal Name")).toHaveValue("Big");
  });

  test("a new deal's account is made in place and picked", async ({ page }) => {
    const server = await serveBoard(page);
    await openBoard(page);
    await page.getByRole("button", { name: "New deal" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "New Account" }).click();
    const panel = dialog.getByRole("group", { name: "New Account" });
    await panel.getByLabel("Company Name").fill("Globex");
    await panel.getByRole("button", { name: "Add account" }).click();
    await expect.poll(() => server.created.length).toBe(1);
    expect(server.created[0]).toMatchObject({ data: { company_name: "Globex" } });
    await expect(panel).toBeHidden();
  });
});
