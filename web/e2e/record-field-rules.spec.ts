/**
 * Field rules in the console (help desk, 2026-11-20): a board moves a card
 * only to the values its record's permissions.moves allow, and a save sends
 * the time the record was read (expected_updated_at); a record locked by a
 * field's value reads as read-only but for that field; a status offers only
 * its moves; a field required in some states is marked once the record
 * reaches one; a date the server stamps is not typed; a person field reads
 * and picks people by name (GET /api/forms/{id}/choices).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const TICKETS = {
  id: "tickets", model_id: "model-1", name: "tickets", label: "Tickets", edit_access: "everyone", delete_access: "admins",
  fields: [
    { name: "code", label: "Ticket", type: "autocode", required: false },
    { name: "title", label: "Title", type: "text", required: true },
    { name: "state", label: "Status", type: "select", required: true, options: ["New", "Open", "Pending", "Resolved", "Closed"],
      transitions: [
        { from: ["New"], to: ["Open", "Resolved"] }, { from: ["Open"], to: ["Pending", "Resolved"] },
        { from: ["Resolved"], to: ["Open", "Closed"] }, { from: ["Closed"], to: ["Open"], roles: ["Support managers"] }],
      locks_in: ["Closed"] },
    { name: "resolution", label: "Resolution", type: "longtext", required: false, required_when: { field: "state", is: ["Resolved", "Closed"] } },
    { name: "resolved_on", label: "Resolved on", type: "date", required: false, stamp_when: { field: "state", is: ["Resolved"] } },
    { name: "assignee", label: "Assignee", type: "person", required: false, roles: ["Agents"], notify: true },
  ],
  created_at: "2026-11-01T09:00:00Z",
  permissions: { sync: false, create: true, create_statuses: [] },
};
const ticket = (id: string, title: string, state: string, permissions: Record<string, unknown>, extra: Record<string, unknown> = {}) => ({
  id, form_id: "tickets", data: { code: id.toUpperCase(), title, state, assignee: "u-olga", ...extra },
  status: "", created_by: "u-1", created_at: "2026-11-01T09:00:00Z", updated_at: `2026-11-0${id.slice(-1)}T10:00:00.123456Z`, permissions,
});

type Server = { puts: { id: string; body: Record<string, unknown> }[] };

async function serve(page: Page): Promise<Server> {
  const server: Server = { puts: [] };
  const records = [
    ticket("t-1", "Printer offline", "New", { edit: true, delete: false, set_status: [], moves: { state: ["Open", "Resolved"] } }),
    ticket("t-2", "VPN down", "Closed", { edit: true, delete: false, set_status: [], moves: { state: [] }, locked_by: "state" },
      { resolution: "Router restarted", resolved_on: "2026-11-02" }),
  ];
  const dashboard = { id: "dash-tickets", name: "Queue", tags: [], widgets: [{ id: "wb", widget_type: "board", ref_id: "tickets", content: null, title: "Queue",
    sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 1000, size_h: 520,
    widget_props: { board: { column_field: "state", card_fields: ["assignee"] }, new_record_button: true } }] };
  await page.route((url) => url.pathname === "/api/dashboards" || url.pathname.startsWith("/api/dashboards/"), (route) =>
    route.fulfill({ json: new URL(route.request().url()).pathname === "/api/dashboards" ? [dashboard] : dashboard }));
  await page.route((url) => url.pathname === "/api/forms", (route) => route.fulfill({ json: [TICKETS] }));
  await page.route((url) => url.pathname === "/api/forms/tickets/choices", (route) => route.fulfill({ json: {
    people: { assignee: [{ id: "u-olga", name: "Olga Agent" }, { id: "u-ivan", name: "Ivan Agent" }] },
    named: { "u-olga": "Olga Agent", "u-ivan": "Ivan Agent" }, inactive: {} } }));
  await page.route((url) => url.pathname === "/api/forms/tickets/board-columns", (route) => route.fulfill({ json: { columns:
    ["New", "Open", "Pending", "Resolved", "Closed"].map(s => ({ key: s, label: s })) } }));
  await page.route((url) => url.pathname === "/api/forms/tickets/records/query", (route) => {
    const q = route.request().postDataJSON() as { group_by?: unknown[] };
    if (q.group_by) {
      return route.fulfill({ json: { kind: "totals", table: { id: "tickets" }, columns: [], rows: [
        { state: "New", count: 1 }, { state: "Closed", count: 1 }], total_groups: 2 } });
    }
    return route.fulfill({ json: { kind: "records", table: { id: "tickets" }, columns: [], records, total: records.length } });
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
  return server;
}

async function openQueue(page: Page) {
  await loadAs(page, "dept_head");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await expect(page.getByRole("listitem", { name: "New" })).toBeVisible();
}

test.describe("field rules", () => {
  test.beforeEach(async ({ page }) => { await mockApi(page); });

  test("a card moves only by its record's moves, sending the time it was read; a locked card does not move", async ({ page }) => {
    const server = await serve(page);
    await openQueue(page);
    const printer = page.getByRole("article", { name: "Printer offline" });
    // The assignee reads by name.
    await expect(printer).toContainText("Olga Agent");
    const moveList = printer.getByLabel("Move Printer offline to");
    await expect(moveList.locator("option", { hasText: "Closed" })).toBeDisabled();
    await expect(moveList.locator("option", { hasText: "Pending" })).toBeDisabled();
    await expect(moveList.locator("option", { hasText: "Open" })).toBeEnabled();
    // A drag to a column it may not move to sends nothing.
    await printer.dragTo(page.getByRole("listitem", { name: "Closed" }));
    await page.waitForTimeout(300);
    expect(server.puts).toHaveLength(0);
    await printer.dragTo(page.getByRole("listitem", { name: "Open" }));
    await expect.poll(() => server.puts.length).toBe(1);
    expect(server.puts[0]).toMatchObject({ id: "t-1", body: { data: { state: "Open" }, expected_updated_at: "2026-11-01T10:00:00.123456Z" } });
    // A closed ticket's card offers no move to an agent.
    await expect(page.getByRole("article", { name: "VPN down" }).getByLabel("Move VPN down to")).toHaveCount(0);
  });

  test("the record's form: moves, a lock, required in a state, a server date, people by name", async ({ page }) => {
    const server = await serve(page);
    await openQueue(page);

    await page.getByRole("article", { name: "VPN down" }).click();
    let dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Edit" }).click();
    await expect(dialog.getByText("Read-only while its Status is Closed: only Status may change.")).toBeVisible();
    await expect(dialog.getByLabel("Title")).toBeDisabled();
    await expect(dialog.getByLabel("Status").locator("option")).toHaveText(["Select…", "Closed"]);
    await page.keyboard.press("Escape");

    await page.getByRole("article", { name: "Printer offline" }).click();
    dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Edit" }).click();
    await expect(dialog.getByLabel("Status").locator("option")).toHaveText(["Select…", "New", "Open", "Resolved"]);
    await expect(dialog.getByLabel("Resolved on")).toHaveValue("Set when Status is Resolved");
    await expect(dialog.getByLabel("Resolved on")).toBeDisabled();
    const resolutionLabel = dialog.locator("label", { hasText: "Resolution" });
    await expect(resolutionLabel).not.toContainText("*");
    await dialog.getByLabel("Status").selectOption("Resolved");
    await expect(resolutionLabel).toContainText("*");
    await expect(dialog.getByLabel("Assignee")).toHaveValue("u-olga");
    await expect(dialog.getByLabel("Assignee").locator("option")).toHaveText(["— nobody —", "Olga Agent", "Ivan Agent"]);
    await dialog.getByLabel("Assignee").selectOption({ label: "Ivan Agent" });
    await dialog.getByLabel("Resolution").fill("Cable replaced");
    await dialog.getByRole("button", { name: "Save changes" }).click();
    await expect.poll(() => server.puts.length).toBe(1);
    expect(server.puts[0]).toMatchObject({ id: "t-1", body: {
      data: { state: "Resolved", assignee: "u-ivan", resolution: "Cable replaced" }, expected_updated_at: "2026-11-01T10:00:00.123456Z" } });
  });
});
