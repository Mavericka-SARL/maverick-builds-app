/**
 * A developer reorders a dimension's members in Build › Dimensions
 * (2026-09-29: sort_order drives every grid, selector and chart, but nobody
 * could change it in the product). Each member row moves up or down among
 * its siblings — the members sharing its parent — and the page sends that
 * parent's whole new child order:
 *
 *   PUT /api/developer/dimensions/{id}/members/order
 *   {"parent_member_id": "<uuid>" | null, "member_ids": [...]}
 *
 * The server renumbers the dimension in tree order; the page then refetches
 * the dimension and every cached grid, so a grid opened before the move shows
 * the new order when you come back to it. A time dimension has no move
 * controls: its order is the calendar.
 *
 * The arrows stay busy until the new order is on screen (a second press built
 * from the old list would undo the first), keep keyboard focus, are off while a
 * member search hides siblings, and in a dimension whose members hang off
 * another's the rows are grouped by that parent so on-screen neighbours are
 * the siblings they move among.
 *
 * Mocked API (CI runs the e2e job without a gateway). The mock keeps the
 * members in server order and renumbers them the way the contract says.
 */
import { test, expect, type Locator, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

type Member = { id: string; code: string; label: string; parent_member_id?: string };

const initialChecklist = (): Member[] => [
  { id: "c-people", code: "PEOPLE", label: "People" },
  { id: "c-people-a", code: "PEOPLE_A", label: "Grant access", parent_member_id: "c-people" },
  { id: "c-people-b", code: "PEOPLE_B", label: "Send invitations", parent_member_id: "c-people" },
  { id: "c-safety", code: "SAFETY", label: "Safety" },
  { id: "c-delivery", code: "DELIVERY", label: "Delivery" },
];

// City hangs off Region: Berlin and Paris are Europe's, Boston is the Americas'.
const REGIONS: Member[] = [
  { id: "r-europe", code: "EUROPE", label: "Europe" },
  { id: "r-americas", code: "AMERICAS", label: "Americas" },
];
const initialCities = (): Member[] => [
  { id: "city-berlin", code: "BERLIN", label: "Berlin", parent_member_id: "r-europe" },
  { id: "city-boston", code: "BOSTON", label: "Boston", parent_member_id: "r-americas" },
  { id: "city-paris", code: "PARIS", label: "Paris", parent_member_id: "r-europe" },
];

const month = (code: string, label: string, i: number) => ({
  id: `p-${code}`, code, label, time_index: i, parent_member_id: "p-q1",
  period_start: `2026-0${i + 1}-01`, period_end: `2026-0${i + 1}-28`,
});
const PERIOD_DIM = {
  id: "period", name: "Period", agg_rule: "sum", dimension_type: "time", time_granularity: "month", fiscal_year_start_month: 1,
  members: [month("JAN", "January", 0), month("FEB", "February", 1), { id: "p-q1", code: "Q1", label: "Quarter one" }],
};

type OrderRequest = { url: string; body: { parent_member_id: string | null; member_ids: string[] } };

type MockOptions = {
  /** Every reorder is refused with this message. */
  refuseWith?: string;
  /** After the first reorder, each dimensions GET answers this many ms late. */
  slowRefetchMs?: number;
  /** Someone else adds this member just before the first reorder arrives. */
  addOnFirstPut?: Member;
};

/**
 * Serves the checklist dimension (nested in itself) and a City dimension whose
 * members hang off Region's, from mutable server state, and answers the
 * reorder route per the contract: member_ids must be exactly the parent's
 * current children, and success renumbers the whole dimension depth-first. A
 * member whose parent sits in another dimension counts as top-level; when such
 * a group is reordered, its members take the positions the group held.
 */
async function mockMemberOrder(page: Page, opts: MockOptions = {}) {
  const dims: Record<string, Member[]> = { checklist: initialChecklist(), region: REGIONS, city: initialCities() };
  const requests: OrderRequest[] = [];
  let delayMs = 0;
  const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
  const serve = () => [
    { id: "checklist", name: "Checklist", agg_rule: "sum", dimension_type: "standard", members: dims.checklist },
    PERIOD_DIM,
    { id: "region", name: "Region", agg_rule: "sum", dimension_type: "standard", members: dims.region },
    { id: "city", name: "City", agg_rule: "sum", dimension_type: "standard", parent_dimension_id: "region", members: dims.city },
  ];

  await page.route("**/api/developer/dimensions*", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const body = serve();
    if (delayMs) await new Promise((r) => setTimeout(r, delayMs));
    return route.fulfill(json(body));
  });
  await page.route("**/api/developer/dimensions/*/members/order*", (route) => {
    const req = route.request();
    const body = req.postDataJSON() as OrderRequest["body"];
    requests.push({ url: req.url(), body });
    if (req.method() !== "PUT") return route.fulfill(json({ error: "method not allowed" }, 405));
    if (opts.refuseWith) return route.fulfill(json({ error: opts.refuseWith }, 400));
    const dimId = new URL(req.url()).pathname.split("/")[4];
    if (opts.addOnFirstPut && requests.length === 1) dims[dimId] = [...dims[dimId], opts.addOnFirstPut];
    const members = dims[dimId];
    const parent = body.parent_member_id ?? undefined;
    const current = members.filter((m) => m.parent_member_id === parent).map((m) => m.id);
    if ([...current].sort().join() !== [...body.member_ids].sort().join()) {
      return route.fulfill(json({ error: "member_ids must be exactly the current children" }, 400));
    }
    const byId = (id: string) => members.find((m) => m.id === id)!;
    const inDim = new Set(members.map((m) => m.id));
    let top = members.filter((m) => !m.parent_member_id || !inDim.has(m.parent_member_id));
    if (parent === undefined || !inDim.has(parent)) {
      let k = 0;
      top = top.map((m) => (m.parent_member_id === parent ? byId(body.member_ids[k++]) : m));
    }
    const childrenOf = (p: string) => (p === parent ? body.member_ids.map(byId) : members.filter((m) => m.parent_member_id === p));
    const next: Member[] = [];
    const walk = (m: Member) => { next.push(m); for (const c of childrenOf(m.id)) walk(c); };
    top.forEach(walk);
    dims[dimId] = next;
    delayMs = opts.slowRefetchMs ?? 0;
    return route.fulfill(json({ status: "ok" }));
  });

  // Business Admin's access-rule picker lists the checklist's members in order.
  await page.route("**/api/business-admin/available*", (route) =>
    new URL(route.request().url()).searchParams.get("type") === "dimension_members"
      ? route.fulfill(json(dims.checklist.map((m) => ({ id: m.id, name: m.label, group: "Checklist" }))))
      : route.fallback());

  // The Run side: a dashboard whose one grid lays the checklist out as rows.
  const DASH_ID = "reorder-dash";
  const dash = {
    id: DASH_ID, name: "Checklist Dashboard", tags: [],
    widgets: [{
      id: "w-checklist", widget_type: "grid", ref_id: "reorder-grid", content: null, sort_order: 0, col_start: 1, col_span: 12,
      widget_props: { default_view: { rows: ["checklist"], cols: ["__metrics__"], context: [] } },
    }],
  };
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dash])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dash)));
  await page.route("**/api/grid*", (route) => route.fulfill(json({
    scenario: "Working", version: "draft",
    metrics: [{ id: "m-done", name: "done", label: "Done", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 }],
    dimensions: [{
      id: "checklist", name: "Checklist",
      members: dims.checklist.map(({ parent_member_id, ...m }) => ({
        ...m, ...(parent_member_id ? { parent_code: dims.checklist.find((p) => p.id === parent_member_id)?.code } : {}),
      })),
    }],
    departments: [], cells: {}, totals: {}, access_rules: { dim_members: {}, metrics: {} },
  })));
  return requests;
}

/**
 * Waits until each label first appears in the element's text in the given
 * order. Polled: a move's refetch lands after the click resolves, so the click
 * alone does not mark "done".
 */
async function expectInOrder(el: Locator, labels: string[]) {
  await expect.poll(async () => {
    const text = await el.innerText();
    const at = labels.map((l) => text.indexOf(l));
    return at.every((pos, i) => pos >= 0 && (i === 0 || pos > at[i - 1]));
  }, { message: `order of ${labels.join(", ")}`, timeout: 10_000 }).toBe(true);
}

const move = (page: Page, label: string, dir: "up" | "down") =>
  page.getByRole("button", { name: `Move member ${label} ${dir}`, exact: true });

async function openDimensions(page: Page) {
  await page.getByRole("button", { name: "Dimensions", exact: true }).click();
  const table = page.locator("table", { hasText: "Send invitations" }).first();
  await expect(table).toBeVisible({ timeout: 15_000 });
  return table;
}

test("a developer moves members among their siblings, and the page sends the parent's whole order", async ({ page }) => {
  await mockApi(page);
  const requests = await mockMemberOrder(page);
  await loadAs(page, "developer");
  const table = await openDimensions(page);
  await expectInOrder(table, ["People", "Grant access", "Send invitations", "Safety", "Delivery"]);

  // The first sibling cannot move up, the last cannot move down — among the
  // roots and among People's children alike.
  await expect(move(page, "People", "up")).toBeDisabled();
  await expect(move(page, "Delivery", "down")).toBeDisabled();
  await expect(move(page, "Grant access", "up")).toBeDisabled();
  await expect(move(page, "Send invitations", "down")).toBeDisabled();
  await expect(move(page, "Safety", "up")).toBeEnabled();

  // A root: the roots' new order, with no parent.
  await move(page, "Safety", "up").click();
  await expect.poll(() => requests.length).toBe(1);
  expect(new URL(requests[0].url).pathname).toBe("/api/developer/dimensions/checklist/members/order");
  expect(requests[0].body).toEqual({ parent_member_id: null, member_ids: ["c-safety", "c-people", "c-delivery"] });
  await expectInOrder(table, ["Safety", "People", "Grant access", "Send invitations", "Delivery"]);
  await expect(move(page, "Safety", "up")).toBeDisabled();
  await expect(move(page, "People", "up")).toBeEnabled();

  // A child: its parent's children, in the new order.
  await move(page, "Send invitations", "up").click();
  await expect.poll(() => requests.length).toBe(2);
  expect(requests[1].body).toEqual({ parent_member_id: "c-people", member_ids: ["c-people-b", "c-people-a"] });
  await expectInOrder(table, ["Safety", "People", "Send invitations", "Grant access", "Delivery"]);
  await expect(move(page, "Send invitations", "up")).toBeDisabled();
  await expect(move(page, "Grant access", "up")).toBeEnabled();

  // Down works the same way.
  await move(page, "People", "down").click();
  await expect.poll(() => requests.length).toBe(3);
  expect(requests[2].body).toEqual({ parent_member_id: null, member_ids: ["c-safety", "c-delivery", "c-people"] });
  await expectInOrder(table, ["Safety", "Delivery", "People", "Send invitations", "Grant access"]);
  await expect(move(page, "People", "down")).toBeDisabled();
  await expect(move(page, "Delivery", "down")).toBeEnabled();
});

test("a refused reorder shows the server's reason", async ({ page }) => {
  await mockApi(page);
  const requests = await mockMemberOrder(page, { refuseWith: "revision is published: create a new revision to change it" });
  await loadAs(page, "developer");
  const table = await openDimensions(page);

  await move(page, "Safety", "up").click();
  await expect.poll(() => requests.length).toBe(1);
  await expect(page.getByRole("alert").filter({ hasText: "revision is published" })).toBeVisible();
  // Nothing moved.
  await expectInOrder(table, ["People", "Grant access", "Send invitations", "Safety", "Delivery"]);
});

test("a time dimension has no move controls and says why", async ({ page }) => {
  await mockApi(page);
  await mockMemberOrder(page);
  await loadAs(page, "developer");
  await openDimensions(page);

  const timeCard = page.locator(".mvx-admin-object", { hasText: "February" });
  await expect(timeCard).toBeVisible();
  await expect(timeCard.getByText("Periods keep calendar order")).toBeVisible();
  await expect(timeCard.getByRole("button", { name: /^Move / })).toHaveCount(0);
  // The standard dimension beside it does have them.
  await expect(move(page, "Safety", "up")).toBeVisible();
});

test("a grid opened before a reorder shows the new order when you return to it", async ({ page }) => {
  await mockApi(page);
  await mockMemberOrder(page);
  // Someone who both runs and builds the app: Run and Build in one console.
  const actor = { user_id: "u-1", email: "alex@example.com", display_name: "Alex", roles: ["business_user", "developer"] };
  await page.route("**/api/me", (route) => route.fulfill({ json: actor }));
  await page.route("**/api/demo", (route) => route.fulfill({ json: {
    app_id: "app-1", model_id: "model-1", revision_id: "rev-1", revision: "FY2026 Budget", scenario: "FY2026 Budget", version: "draft", actor,
  } }));
  await loadAs(page, "developer");

  const runDashboards = page.getByRole("button", { name: "Dashboards", exact: true }).first();
  await runDashboards.click();
  const grid = page.locator("table", { hasText: "Send invitations" }).first();
  await expect(grid).toBeVisible({ timeout: 15_000 });
  await expectInOrder(grid, ["People", "Grant access", "Send invitations", "Safety", "Delivery"]);

  const table = await openDimensions(page);
  await move(page, "Delivery", "up").click();
  await expectInOrder(table, ["People", "Send invitations", "Delivery", "Safety"]);

  // Back in Run the grid is refetched, not served from its 30-second cache.
  await runDashboards.click();
  await expect(grid).toBeVisible({ timeout: 15_000 });
  await expectInOrder(grid, ["People", "Grant access", "Send invitations", "Delivery", "Safety"]);
});

test("a second move waits for the first one's new order instead of undoing it", async ({ page }) => {
  await mockApi(page);
  const requests = await mockMemberOrder(page, { slowRefetchMs: 1_500 });
  await loadAs(page, "developer");
  const table = await openDimensions(page);

  // Pressing Up twice moves Delivery two places. Until the refetched order is
  // on screen the arrows are busy, so the second press is built from the new
  // order, not the old one it would otherwise send again.
  await move(page, "Delivery", "up").click();
  await expect.poll(() => requests.length).toBe(1);
  await expect(move(page, "Delivery", "up")).toBeDisabled();
  await expect(move(page, "People", "down")).toBeDisabled();
  await move(page, "Delivery", "up").click();
  await expect.poll(() => requests.length).toBe(2);
  expect(requests[0].body).toEqual({ parent_member_id: null, member_ids: ["c-people", "c-delivery", "c-safety"] });
  expect(requests[1].body).toEqual({ parent_member_id: null, member_ids: ["c-delivery", "c-people", "c-safety"] });
  await expectInOrder(table, ["Delivery", "People", "Grant access", "Send invitations", "Safety"]);
});

test("a keyboard user keeps focus on the arrow they pressed", async ({ page }) => {
  await mockApi(page);
  await mockMemberOrder(page, { slowRefetchMs: 300 });
  await loadAs(page, "developer");
  const table = await openDimensions(page);

  await move(page, "Delivery", "up").focus();
  await page.keyboard.press("Enter");
  await expectInOrder(table, ["People", "Send invitations", "Delivery", "Safety"]);
  await expect(move(page, "Delivery", "up")).toBeFocused();

  // Enter again moves it on; at the top its Up arrow is off, so Down takes focus.
  await page.keyboard.press("Enter");
  await expectInOrder(table, ["Delivery", "People", "Send invitations", "Safety"]);
  await expect(move(page, "Delivery", "down")).toBeFocused();
});

test("members of a dimension that hangs off another are listed, and moved, within their parent", async ({ page }) => {
  await mockApi(page);
  const requests = await mockMemberOrder(page);
  await loadAs(page, "developer");
  await openDimensions(page);

  // Server order is Berlin, Boston, Paris; on screen each Region's members sit together.
  const city = page.locator(".mvx-admin-object", { hasText: "Boston" });
  await expect(city.getByText("the arrows move a member within its Region")).toBeVisible();
  await expectInOrder(city, ["Region: Europe", "Berlin", "Paris", "Region: Americas", "Boston"]);
  await expect(move(page, "Berlin", "up")).toBeDisabled();
  await expect(move(page, "Paris", "down")).toBeDisabled();
  await expect(move(page, "Boston", "up")).toBeDisabled();
  await expect(move(page, "Boston", "down")).toBeDisabled();

  await move(page, "Paris", "up").click();
  await expect.poll(() => requests.length).toBe(1);
  expect(new URL(requests[0].url).pathname).toBe("/api/developer/dimensions/city/members/order");
  expect(requests[0].body).toEqual({ parent_member_id: "r-europe", member_ids: ["city-paris", "city-berlin"] });
  // The server now orders Paris, Boston, Berlin; the page still groups them.
  await expectInOrder(city, ["Region: Europe", "Paris", "Berlin", "Region: Americas", "Boston"]);
  await expect(move(page, "Paris", "up")).toBeDisabled();
  await expect(move(page, "Berlin", "up")).toBeEnabled();
});

test("while a member search is on, the arrows are off and say why", async ({ page }) => {
  await mockApi(page);
  const requests = await mockMemberOrder(page);
  await loadAs(page, "developer");
  await openDimensions(page);
  // Located by the dimension's name: the search hides the other members.
  const card = page.locator(".mvx-admin-object", { has: page.locator("code", { hasText: /^Checklist$/ }) });

  // Only Delivery shows; Up would otherwise swap it with the hidden Safety.
  await card.getByPlaceholder("Search members…").fill("Delivery");
  await expect(card.locator("table")).not.toContainText("Safety");
  await expect(move(page, "Delivery", "up")).toBeDisabled();
  await expect(move(page, "Delivery", "up")).toHaveAttribute("title", "Clear the search to reorder");
  await expect(card.getByText("clear the search to reorder")).toBeVisible();

  await card.getByPlaceholder("Search members…").fill("");
  await expect(move(page, "Delivery", "up")).toBeEnabled();
  expect(requests).toHaveLength(0);
});

test("a reorder refused because the children changed refetches them, so the next move works", async ({ page }) => {
  await mockApi(page);
  const requests = await mockMemberOrder(page, { addOnFirstPut: { id: "c-training", code: "TRAINING", label: "Training" } });
  await loadAs(page, "developer");
  const table = await openDimensions(page);

  // Someone else added Training; the move built without it is refused, and
  // the list refetches rather than waiting out its 30-second cache.
  await move(page, "Safety", "up").click();
  await expect(page.getByRole("alert").filter({ hasText: "exactly the current children" })).toBeVisible();
  await expectInOrder(table, ["People", "Safety", "Delivery", "Training"]);

  await move(page, "Safety", "up").click();
  await expect.poll(() => requests.length).toBe(2);
  expect(requests[1].body).toEqual({ parent_member_id: null, member_ids: ["c-safety", "c-people", "c-delivery", "c-training"] });
  await expectInOrder(table, ["Safety", "People", "Grant access", "Delivery", "Training"]);
  await expect(page.getByRole("alert").filter({ hasText: "exactly the current children" })).toHaveCount(0);
});

test("the access-rule member picker shows the new order when a developer who is also business admin returns to it", async ({ page }) => {
  await mockApi(page);
  await mockMemberOrder(page);
  // Builds the app and administers its access: both sidebar groups, one console.
  const actor = { user_id: "u-1", email: "alex@example.com", display_name: "Alex", roles: ["developer", "business_admin"] };
  await page.route("**/api/me", (route) => route.fulfill({ json: actor }));
  await page.route("**/api/demo", (route) => route.fulfill({ json: {
    app_id: "app-1", model_id: "model-1", revision_id: "rev-1", revision: "FY2026 Budget", scenario: "FY2026 Budget", version: "draft", actor,
  } }));
  await loadAs(page, "developer");

  const accessRules = page.getByRole("button", { name: "Access Rules", exact: true });
  const openRules = async () => {
    await accessRules.click();
    await page.getByLabel("Select user").selectOption("u-2");
    const rules = page.locator("table", { hasText: "Send invitations" }).first();
    await expect(rules).toBeVisible({ timeout: 15_000 });
    return rules;
  };
  await expectInOrder(await openRules(), ["People", "Grant access", "Send invitations", "Safety", "Delivery"]);

  const table = await openDimensions(page);
  await move(page, "Delivery", "up").click();
  await expectInOrder(table, ["People", "Send invitations", "Delivery", "Safety"]);

  // Refetched, not served from its 30-second cache.
  await expectInOrder(await openRules(), ["People", "Grant access", "Send invitations", "Delivery", "Safety"]);
});
