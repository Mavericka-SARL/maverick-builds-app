/**
 * Records widgets following the dashboard's selection, against a stubbed
 * server that records what the console sends — journals above, their lines
 * below:
 *
 *  - a records widget with record_selects selects the clicked record's member
 *    (the table keeps the Journal dimension): the row is marked, nothing opens,
 *    and the eye beside it opens the record;
 *  - a records widget with record_follows ["journal"] asks the server only for
 *    the selected journal's lines, says so in its header, and its + New starts
 *    with that journal; before any selection it lists every line;
 *  - a parent member selected follows as the leaves under it.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

type Body = Record<string, unknown>;

const DIMS = [{
  id: "dim-journal", name: "Journal", members: [
    { id: "m-all", code: "ALL", label: "All journals", parent_member_id: null },
    { id: "m-q3", code: "Q3", label: "Third quarter", parent_member_id: "m-all" },
    { id: "m-j1", code: "J-001", label: "Accrual", parent_member_id: "m-q3" },
    { id: "m-j2", code: "J-002", label: "Reclass", parent_member_id: "m-q3" },
  ],
}];

const perms = { sync: false, create: true, create_statuses: [] };
const JOURNALS = {
  id: "form-j", model_id: "model-1", name: "journal", label: "Journals", created_at: "2026-05-01T09:00:00Z", keeps_dimension_id: "dim-journal",
  fields: [
    { name: "code", label: "Code", type: "autocode", required: false, code_prefix: "J-", code_digits: 3 },
    { name: "title", label: "Title", type: "text", required: true },
  ],
  permissions: perms,
};
const LINES = {
  id: "form-l", model_id: "model-1", name: "line", label: "Lines", created_at: "2026-05-01T09:00:00Z",
  fields: [
    { name: "journal", label: "Journal", type: "dimension", required: true, dimension_id: "dim-journal" },
    { name: "memo", label: "Memo", type: "text", required: false },
    { name: "amount", label: "Amount", type: "number", required: true },
  ],
  permissions: perms,
};
const rec = (id: string, form: string, data: Body) => ({ id, form_id: form, data, status: "", created_by: "u-1",
  created_at: "2026-09-20T09:00:00Z", updated_at: "2026-09-20T09:00:00Z", permissions: { edit: true, delete: true, set_status: [] } });
const JOURNAL_RECORDS = [rec("j1", "form-j", { code: "J-001", title: "Accrual" }), rec("j2", "form-j", { code: "J-002", title: "Reclass" })];
const LINE_RECORDS = [
  rec("l1", "form-l", { journal: "J-001", memo: "accrual debit", amount: 10 }),
  rec("l2", "form-l", { journal: "J-001", memo: "accrual credit", amount: -10 }),
  rec("l3", "form-l", { journal: "J-002", memo: "reclass debit", amount: 5 }),
];

type Server = { lineQueries: Body[]; posts: Body[] };
const url = (route: { request(): { url(): string } }) => new URL(route.request().url()).pathname;

async function serve(page: Page, selectorBar?: Body[], intro = false): Promise<Server> {
  const server: Server = { lineQueries: [], posts: [] };
  await page.route((url) => url.pathname === "/api/forms", (route) => route.fulfill({ json: [JOURNALS, LINES] }));
  await page.route((url) => url.pathname === "/api/dimensions", (route) => route.fulfill({ json: DIMS }));
  await page.route((url) => url.pathname === "/api/forms/form-j/records/query", (route) =>
    route.fulfill({ json: { kind: "records", table: { id: "form-j", name: "journal", label: "Journals" }, columns: [], records: JOURNAL_RECORDS, total: 2 } }));
  await page.route((url) => url.pathname === "/api/forms/form-l/records/query", (route) => {
    const q = route.request().postDataJSON() as { filters?: { field: string; op: string; value?: string; values?: string[] }[] };
    server.lineQueries.push(q);
    const f = (q.filters ?? []).find(x => x.field === "journal");
    const keep = (j: string) => !f || (f.op === "eq" ? j === f.value : (f.values ?? []).includes(j));
    const records = LINE_RECORDS.filter(r => keep(String(r.data.journal)));
    return route.fulfill({ json: { kind: "records", table: { id: "form-l", name: "line", label: "Lines" }, columns: [], records, total: records.length } });
  });
  await page.route((url) => url.pathname === "/api/forms/form-l/records", (route) => {
    server.posts.push(route.request().postDataJSON());
    return route.fulfill({ json: { ...LINE_RECORDS[2], id: "l4" } });
  });
  // The record page: the record, the records naming it, its history.
  await page.route((url) => /^\/api\/records\/[^/]+\/related$/.test(url.pathname), (route) => route.fulfill({ json: { related: [] } }));
  await page.route((url) => /^\/api\/records\/[^/]+\/history$/.test(url.pathname), (route) => route.fulfill({ json: { history: [] } }));
  await page.route((url) => /^\/api\/records\/[^/]+$/.test(url.pathname), (route) =>
    route.fulfill({ json: [...JOURNAL_RECORDS, ...LINE_RECORDS].find(r => url(route).endsWith("/" + r.id)) ?? JOURNAL_RECORDS[0] }));
  const dashboard = { id: "dash-3", name: "OPEX form 2", tags: ["finance"], folder_id: null, widgets: [
    { id: "w-j", widget_type: "form", ref_id: "form-j", content: null, sort_order: 0, col_start: 1, col_span: 12, widget_props: { record_selects: true } },
    { id: "w-l", widget_type: "form", ref_id: "form-l", content: null, sort_order: 1, col_start: 1, col_span: 12,
      widget_props: { record_follows: ["journal"], new_record_button: true } },
  ] };
  // A text above the records, on rows of their own: the bar stands below it.
  if (intro) dashboard.widgets = [
    { id: "w-t", widget_type: "text", ref_id: null as unknown as string, content: "Journals and their lines", sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 1100, size_h: 60 } as never,
    ...dashboard.widgets.map((w, i) => ({ ...w, pos_x: 0, pos_y: 100 + i * 400, size_w: 1100, size_h: 360 })),
  ];
  await page.route((url) => url.pathname === "/api/dashboards/dash-3", (route) => route.fulfill({ json: { ...dashboard, selector_bar: selectorBar } }));
  // The selector bar's members: the model-wide grid meta.
  const barDims = DIMS.map(d => ({ id: d.id, name: d.name, members: d.members.map(m => ({ id: m.id, code: m.code, label: m.label,
    parent_code: d.members.find(p => p.id === m.parent_member_id)?.code })) }));
  await page.route("**/api/grid*meta_only*", (route) => route.fulfill({ json: { scenario: "Working", version: "draft", metrics: [], dimensions: barDims,
    all_dimensions: barDims, departments: [], cells: {}, totals: {}, access_rules: { dim_members: {}, metrics: {} } } }));
  return server;
}

const panel = (page: Page, title: string) => page.locator(".mvx-records-fill").filter({ has: page.locator(".mvx-panel-head__title", { hasText: title }) });
const lastJournalFilter = (s: Server) => JSON.stringify((s.lineQueries[s.lineQueries.length - 1]?.filters as Body[] | undefined) ?? []);

async function open(page: Page) {
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "OPEX form 2" }).first().click();
  await expect(panel(page, "Lines").getByRole("row").filter({ hasText: "reclass debit" })).toBeVisible();
}

test("a journal clicked selects it, and the lines below are its own", async ({ page }) => {
  await mockApi(page);
  const server = await serve(page);
  await loadAs(page, "dept_head");
  await open(page);
  // Nothing selected: every line.
  expect(lastJournalFilter(server)).toBe("[]");
  await expect(panel(page, "Lines").getByRole("row").filter({ hasText: "accrual debit" })).toBeVisible();

  const journals = panel(page, "Journals");
  const j2 = journals.getByRole("row").filter({ hasText: "J-002" });
  await j2.getByText("Reclass").click();
  await expect(j2).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect.poll(() => lastJournalFilter(server)).toBe(JSON.stringify([{ field: "journal", op: "eq", value: "J-002" }]));
  const lines = panel(page, "Lines");
  await expect(lines.getByRole("row").filter({ hasText: "reclass debit" })).toBeVisible();
  await expect(lines.getByRole("row").filter({ hasText: "accrual debit" })).toHaveCount(0);
  await expect(lines.getByLabel("Follows the dashboard's selection: Journal is Reclass")).toBeVisible();

  // + New starts with the selected journal.
  await lines.getByRole("button", { name: /New line/ }).click();
  await expect(page.getByLabel("Journal", { exact: true })).toContainText("Reclass");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();

  // Another journal: its lines; the eye opens a journal without selecting it.
  const j1 = journals.getByRole("row").filter({ hasText: "J-001" });
  await j1.getByText("Accrual").click();
  await expect(j1).toHaveAttribute("aria-selected", "true");
  await expect(j2).toHaveAttribute("aria-selected", "false");
  await expect.poll(() => lastJournalFilter(server)).toBe(JSON.stringify([{ field: "journal", op: "eq", value: "J-001" }]));
  await j2.getByRole("button", { name: "Open record" }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(j1).toHaveAttribute("aria-selected", "true");
});

test("the selector bar's journal drives the lines too: a parent, the leaves under it", async ({ page }) => {
  await mockApi(page);
  const server = await serve(page, [{ dimension_id: "dim-journal", default: "Q3" }]);
  await loadAs(page, "dept_head");
  await open(page);
  await expect.poll(() => lastJournalFilter(server)).toBe(JSON.stringify([{ field: "journal", op: "in", values: ["Q3", "J-001", "J-002"] }]));

  const barJournal = page.getByRole("toolbar", { name: "Dashboard selectors" }).getByRole("button", { name: "Journal", exact: true });
  await barJournal.click();
  await page.getByRole("option", { name: "Reclass" }).click();
  await expect.poll(() => lastJournalFilter(server)).toBe(JSON.stringify([{ field: "journal", op: "eq", value: "J-002" }]));
  await expect(panel(page, "Journals").getByRole("row").filter({ hasText: "J-002" })).toHaveAttribute("aria-selected", "true");

  // A journal clicked moves the bar with it.
  await panel(page, "Journals").getByRole("row").filter({ hasText: "J-001" }).getByText("Accrual").click();
  await expect(barJournal).toHaveText("Accrual");
});

// The bar stands above the first widget following the selection it drives —
// a records widget following or making it too (it stood half-way down a
// project dashboard, above its first chart, 2026-11-27); a text above stays
// above it.
test("the selector bar stands above the records that follow it, below a text", async ({ page }) => {
  await mockApi(page);
  await serve(page, [{ dimension_id: "dim-journal", default: "Q3" }], true);
  await loadAs(page, "dept_head");
  await open(page);
  const bar = page.getByRole("toolbar", { name: "Dashboard selectors" });
  await expect(bar).toBeVisible();
  const intro = page.getByText("Journals and their lines");
  await expect.poll(async () => {
    const [b, t, j] = [await bar.boundingBox(), await intro.boundingBox(), await panel(page, "Journals").boundingBox()];
    return !!b && !!t && !!j && b.y > t.y + t.height - 1 && b.y + b.height <= j.y + 1;
  }).toBe(true);
});
