/**
 * A grid shows a dimension's members in the dimension's own order, not by
 * code (2026-09-29, live on the Tenant admin guide checklist: sort_order is
 * PEOPLE 1, SAFETY 4, DELIVERY 7, IDENTITY 10, but the grid showed DELIVERY,
 * IDENTITY, PEOPLE, SAFETY). The server sends members in that order — time
 * period, then sort_order, then code — and the hierarchical row and column
 * axes, the context selectors and Build › Dimensions used to re-sort them by
 * code. (sort_order is set by imports, integrations, the AI assistant's
 * reorder_dimension_members, and the Move up / Move down arrows on each member
 * row in Build › Dimensions.)
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const DASH_ID = "member-order-dash";
const GRID_ID = "member-order-grid";

// Server order (sort_order, then code). Codes sort differently on purpose:
// groups DELIVERY < IDENTITY < PEOPLE < SAFETY, and within a group the
// "_A" member sorts before "_B" although "_B" comes first.
const checklist = [
  { id: "c-people", code: "PEOPLE", label: "People" },
  { id: "c-people-b", code: "PEOPLE_B", label: "Grant access", parent_code: "PEOPLE" },
  { id: "c-people-a", code: "PEOPLE_A", label: "Send invitations", parent_code: "PEOPLE" },
  { id: "c-safety", code: "SAFETY", label: "Safety" },
  { id: "c-safety-b", code: "SAFETY_B", label: "Set retention", parent_code: "SAFETY" },
  { id: "c-safety-a", code: "SAFETY_A", label: "Review audit log", parent_code: "SAFETY" },
  { id: "c-delivery", code: "DELIVERY", label: "Delivery" },
  { id: "c-delivery-a", code: "DELIVERY_A", label: "Configure e-mail", parent_code: "DELIVERY" },
  { id: "c-identity", code: "IDENTITY", label: "Identity" },
  { id: "c-identity-a", code: "IDENTITY_A", label: "Connect SSO", parent_code: "IDENTITY" },
];

// A context dimension: its first member in the dimension's order is the
// default context, and the selector lists members in that order.
const team = [
  { id: "t-zulu", code: "ZULU", label: "Zulu team" },
  { id: "t-alpha", code: "ALPHA", label: "Alpha team" },
];

// A time dimension as the server sends it: dated leaves by time_index, then
// the undated quarters. Month codes sort non-chronologically (APR < FEB <
// JAN < JUN < MAR < MAY).
const month = (code: string, label: string, i: number, q: string) => ({
  id: `p-${code}`, code, label, parent_code: q, time_index: i,
  period_start: `2026-0${i + 1}-01`, period_end: `2026-0${i + 1}-28`,
});
const period = [
  month("JAN", "January", 0, "Q1"), month("FEB", "February", 1, "Q1"), month("MAR", "March", 2, "Q1"),
  month("APR", "April", 3, "Q2"), month("MAY", "May", 4, "Q2"), month("JUN", "June", 5, "Q2"),
  { id: "p-q1", code: "Q1", label: "Quarter one" },
  { id: "p-q2", code: "Q2", label: "Quarter two" },
];

// A hierarchical context dimension: WEST (sort_order 1) before EAST, and
// each region's cities out of code order.
const region = [
  { id: "r-west", code: "WEST", label: "West" },
  { id: "r-zeta", code: "ZETA_CITY", label: "Zeta City", parent_code: "WEST" },
  { id: "r-alpha", code: "ALPHA_CITY", label: "Alpha City", parent_code: "WEST" },
  { id: "r-east", code: "EAST", label: "East" },
  { id: "r-york", code: "YORK", label: "York", parent_code: "EAST" },
  { id: "r-boston", code: "BOSTON", label: "Boston", parent_code: "EAST" },
];

type GridDim = { id: string; name: string; members: unknown[]; dimension_type?: string; time_granularity?: string };
const CHECKLIST_DIM: GridDim = { id: "checklist", name: "Checklist", members: checklist };
const TEAM_DIM: GridDim = { id: "team", name: "Team", members: team };
const PERIOD_DIM: GridDim = { id: "period", name: "Period", members: period, dimension_type: "time", time_granularity: "month" };
const REGION_DIM: GridDim = { id: "region", name: "Region", members: region };

const gridData = (dimensions: GridDim[]) => ({
  scenario: "Working",
  version: "draft",
  metrics: [{ id: "m-done", name: "done", label: "Done", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0 }],
  dimensions,
  departments: [],
  cells: {},
  totals: {},
  access_rules: { dim_members: {}, metrics: {} },
});

async function mockOrderGrid(page: Page, view: { rows: string[]; cols: string[]; context: string[] }, dimensions: GridDim[] = [CHECKLIST_DIM, TEAM_DIM]) {
  const dash = {
    id: DASH_ID,
    name: "Member Order Dashboard",
    tags: [],
    widgets: [{
      id: "w-order", widget_type: "grid", ref_id: GRID_ID, content: null, sort_order: 0, col_start: 1, col_span: 12,
      widget_props: { default_view: view },
    }],
  };
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/dashboards", (route) => route.fulfill(json([dash])));
  await page.route(`**/api/dashboards/${DASH_ID}`, (route) => route.fulfill(json(dash)));
  await page.route("**/api/grid*", (route) => route.fulfill(json(gridData(dimensions))));
}

/** Asserts each label first appears in `text` in the given order. */
function expectInOrder(text: string, labels: string[]) {
  const at = labels.map((l) => text.indexOf(l));
  for (const [i, pos] of at.entries()) expect(pos, `"${labels[i]}" is shown`).toBeGreaterThanOrEqual(0);
  expect(at, `order of ${labels.join(", ")}`).toEqual([...at].sort((a, b) => a - b));
}

const ROW_ORDER = [
  "People", "Grant access", "Send invitations",
  "Safety", "Set retention", "Review audit log",
  "Delivery", "Configure e-mail",
  "Identity", "Connect SSO",
];

test("hierarchical rows follow the dimension's order, and the context defaults to its first member", async ({ page }) => {
  await mockApi(page);
  await mockOrderGrid(page, { rows: ["checklist"], cols: ["__metrics__"], context: ["team"] });
  await loadAs(page, "dept_head");
  await expect(page.getByText("Member Order Dashboard")).toBeVisible({ timeout: 15_000 });

  const table = page.locator("table").first();
  await expect(table).toContainText("Connect SSO");
  expectInOrder(await table.innerText(), ROW_ORDER);

  // The context selector: first member by default, options in order.
  const selector = page.getByRole("button", { name: "Team context" });
  await expect(selector).toHaveText("Zulu team");
  await selector.click();
  await expect(page.getByRole("listbox").getByRole("option")).toHaveText(["Zulu team", "Alpha team"]);
});

test("hierarchical column headers follow the dimension's order", async ({ page }) => {
  await mockApi(page);
  await mockOrderGrid(page, { rows: ["__metrics__"], cols: ["checklist"], context: ["team"] });
  await loadAs(page, "dept_head");
  await expect(page.getByText("Member Order Dashboard")).toBeVisible({ timeout: 15_000 });

  const head = page.locator("table").first().locator("thead");
  await expect(head).toContainText("Connect SSO");
  // Group headers, then each group's leaves.
  expectInOrder(await head.innerText(), ["People", "Safety", "Delivery", "Identity"]);
  expectInOrder(await head.innerText(), ["Grant access", "Send invitations", "Set retention", "Review audit log", "Configure e-mail", "Connect SSO"]);
});

test("a time dimension's months stay in period order under their quarters", async ({ page }) => {
  await mockApi(page);
  await mockOrderGrid(page, { rows: ["__metrics__"], cols: ["period"], context: ["team"] }, [PERIOD_DIM, TEAM_DIM]);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Member Order Dashboard")).toBeVisible({ timeout: 15_000 });

  const head = page.locator("table").first().locator("thead");
  await expect(head).toContainText("June");
  expectInOrder(await head.innerText(), ["Quarter one", "Quarter two"]);
  expectInOrder(await head.innerText(), ["January", "February", "March", "April", "May", "June"]);
});

test("a hierarchical context selector lists members in the dimension's order and defaults to its first leaf", async ({ page }) => {
  await mockApi(page);
  await mockOrderGrid(page, { rows: ["checklist"], cols: ["__metrics__"], context: ["region"] }, [CHECKLIST_DIM, REGION_DIM]);
  await loadAs(page, "dept_head");
  await expect(page.getByText("Member Order Dashboard")).toBeVisible({ timeout: 15_000 });

  // The first leaf depth-first, in order: West › Zeta City (by code it was
  // East › Boston).
  const selector = page.getByRole("button", { name: "Region context" });
  await expect(selector).toContainText("Zeta City");
  await selector.click();
  const list = page.getByRole("listbox");
  await expect(list).toContainText("Boston");
  expectInOrder(await list.innerText(), ["West", "Zeta City", "Alpha City", "East", "York", "Boston"]);
});

test("Build › Dimensions lists members in the dimension's order", async ({ page }) => {
  await mockApi(page);
  // The developer API's shape: parent by id.
  await page.route("**/api/developer/dimensions*", (route) => route.fulfill({
    status: 200, contentType: "application/json",
    body: JSON.stringify([{
      id: "checklist", name: "Checklist", agg_rule: "sum",
      members: checklist.map(({ parent_code, ...m }) => ({
        ...m, parent_member_id: parent_code ? checklist.find((p) => p.code === parent_code)?.id : undefined,
      })),
    }]),
  }));
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Dimensions", exact: true }).click();

  const table = page.locator("table", { hasText: "Connect SSO" }).first();
  await expect(table).toBeVisible({ timeout: 15_000 });
  expectInOrder(await table.innerText(), ROW_ORDER);
});

test("Build › Dimensions dims the move arrows while a move is in flight", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/dimensions*", (route) => route.fulfill({
    status: 200, contentType: "application/json",
    body: JSON.stringify([{
      id: "checklist", name: "Checklist", agg_rule: "sum",
      members: checklist.map(({ parent_code, ...m }) => ({
        ...m, parent_member_id: parent_code ? checklist.find((p) => p.code === parent_code)?.id : undefined,
      })),
    }]),
  }));
  // The reorder never answers: the arrows stay busy for the whole test.
  let sent = 0;
  await page.route("**/api/developer/dimensions/checklist/members/order", () => { sent++; });
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Dimensions", exact: true }).click();

  const up = page.getByRole("button", { name: "Move member Safety up" });
  const peopleDown = page.getByRole("button", { name: "Move member People down" });
  await expect(up).toBeVisible({ timeout: 15_000 });
  await expect(peopleDown).toHaveCSS("opacity", "1");
  await up.click();
  await expect.poll(() => sent).toBe(1);

  // aria-disabled, not disabled: the buttons keep focus, and design-system.css
  // gives them :disabled's dimmed look with a "wait" cursor.
  await expect(peopleDown).toHaveAttribute("aria-disabled", "true");
  await expect(peopleDown).toHaveCSS("opacity", "0.55");
  await expect(peopleDown).toHaveCSS("cursor", "progress");

  // A busy arrow does not light up on hover. The background transition starts
  // from transparent, so let it finish before reading the colour.
  await peopleDown.hover();
  await peopleDown.evaluate((el) => Promise.all(el.getAnimations().map((a) => a.finished)));
  await expect(peopleDown).toHaveCSS("background-color", "rgba(0, 0, 0, 0)");
});
