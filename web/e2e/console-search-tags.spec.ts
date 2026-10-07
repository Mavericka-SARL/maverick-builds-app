/**
 * Search and tags on the developer's lists, the draggable sidebar, and the
 * business sidebar group names. The API is mocked (e2e/mocks.ts); the two
 * routes below override it with tagged definitions.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const nav = (page: Page, name: string) => page.getByRole("button", { name, exact: true });
// A card's name is the first <code> in it (member codes come later, in its table).
const cardNames = (page: Page) =>
  page.locator(".mvx-admin-stack > .mvx-admin-object").evaluateAll((cards) => cards.map((c) => c.querySelector("code")?.textContent ?? ""));

const dimensions = [
  {
    id: "d1", name: "department", agg_rule: "sum", dimension_type: "standard", tags: ["org"],
    members: [
      { id: "m-root", code: "OPEX", label: "OPEX" },
      { id: "m-mkt", code: "MKT", label: "Marketing", parent_member_id: "m-root", properties: { owner: "Dana" } },
      { id: "m-sales", code: "SALES", label: "Sales", parent_member_id: "m-root", properties: { owner: "Lee" } },
      { id: "m-ops", code: "OPS", label: "Operations", parent_member_id: "m-root" },
    ],
  },
  {
    id: "d2", name: "region", agg_rule: "sum", dimension_type: "standard",
    members: [
      { id: "r-eu", code: "EU", label: "Europe" },
      { id: "r-na", code: "NA", label: "North America" },
    ],
  },
];

const metrics = [
  { id: "m1", name: "revenue", label: "Revenue", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, format_currency: "$", depends_on: [], depended_by: [], tags: ["sales"] },
  { id: "m2", name: "opex", label: "Opex", is_input: true, agg_rule: "sum", format: "number", format_decimals: 0, format_currency: "$", depends_on: [], depended_by: [], tags: [] },
];

async function withTaggedDefinitions(page: Page) {
  await mockApi(page);
  await page.route((url) => url.pathname === "/api/developer/dimensions", (route) => route.fulfill({ json: dimensions }));
  await page.route((url) => url.pathname === "/api/developer/model", (route) =>
    route.fulfill({ json: { app_name: "Planning", model_name: "Finance Model", model_id: "model-1", metrics } }));
}

test("dimensions: search reaches member codes, names and property values and names; tags filter", async ({ page }) => {
  await withTaggedDefinitions(page);
  await loadAs(page, "developer");
  await nav(page, "Dimensions").click();
  const search = page.getByPlaceholder("Search dimensions, members, properties…");
  await expect(search).toBeVisible();
  await expect.poll(() => cardNames(page)).toEqual(["department", "region"]);

  // A member's code: only its dimension, its tree narrowed to that member.
  await search.fill("mkt");
  await expect.poll(() => cardNames(page)).toEqual(["department"]);
  await expect(page.getByText("Marketing", { exact: true })).toBeVisible();
  await expect(page.getByText("Operations", { exact: true })).toHaveCount(0);

  // A member's name.
  await search.fill("north am");
  await expect.poll(() => cardNames(page)).toEqual(["region"]);

  // A property value narrows to the member holding it.
  await search.fill("dana");
  await expect.poll(() => cardNames(page)).toEqual(["department"]);
  await expect(page.getByText("Marketing", { exact: true })).toBeVisible();
  await expect(page.getByText("Sales", { exact: true })).toHaveCount(0);

  // A property name shows the whole card (the mock declares "owner" on every dimension).
  await search.fill("owner");
  await expect.poll(() => cardNames(page)).toEqual(["department", "region"]);
  await expect(page.getByText("Operations", { exact: true })).toBeVisible();

  await search.fill("nothing-here");
  await expect(page.getByText('No dimension, member or property matches "nothing-here".')).toBeVisible();
  await search.fill("");

  // The toolbar's tag chip filters; choosing it again clears.
  const chip = page.locator(".mvx-toolbar").getByRole("button", { name: "org" });
  await chip.click();
  await expect.poll(() => cardNames(page)).toEqual(["department"]);
  await chip.click();
  await expect.poll(() => cardNames(page)).toEqual(["department", "region"]);
});

test("dimensions: the edit form sends the name with the tags", async ({ page }) => {
  await withTaggedDefinitions(page);
  let patched: unknown = null;
  await page.route((url) => url.pathname === "/api/developer/dimensions/d2", async (route) => {
    patched = route.request().postDataJSON();
    await route.fulfill({ json: { status: "ok" } });
  });
  await loadAs(page, "developer");
  await nav(page, "Dimensions").click();
  await page.getByRole("button", { name: "Edit dimension region" }).click();
  const tag = page.getByRole("textbox", { name: "Add tag" });
  await tag.fill("Geo Areas");
  await tag.press("Enter");
  await tag.fill("typed"); // not entered: taken when the field is left
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => patched).toEqual({ name: "region", tags: ["geo-areas", "typed"] });
});

test("metrics: tag chips and search filter by tag", async ({ page }) => {
  await withTaggedDefinitions(page);
  await loadAs(page, "developer");
  await nav(page, "Metrics").click();
  const rows = page.locator("table.mvx-table tbody tr").filter({ has: page.getByRole("button", { name: /^Edit metric / }) });
  await expect(rows).toHaveCount(2);
  await page.locator(".mvx-toolbar").getByRole("button", { name: "sales" }).click();
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("revenue");
  await page.locator(".mvx-toolbar").getByRole("button", { name: "sales" }).click();
  await page.getByPlaceholder("Search metrics…").fill("sales");
  await expect(rows).toHaveCount(1);
});

test("grids: tag chips filter, and the edit form sends the name with the tags", async ({ page }) => {
  await mockApi(page);
  const gridRows = [
    { id: "g1", name: "OPEX Planning Grid", tags: ["opex", "plan"], revision_id: "rev-1", metric_ids: [], dimension_ids: [], dimension_levels: {} },
    { id: "g2", name: "Headcount Grid", tags: ["people"], revision_id: "rev-1", metric_ids: [], dimension_ids: [], dimension_levels: {} },
  ];
  let patched: unknown = null;
  await page.route((url) => url.pathname === "/api/developer/grids", (route) => route.fulfill({ json: gridRows }));
  await page.route((url) => url.pathname === "/api/developer/grids/g1", async (route) => {
    patched = route.request().postDataJSON();
    await route.fulfill({ json: { status: "ok" } });
  });
  await loadAs(page, "developer");
  await nav(page, "Grids").click();
  const names = page.locator(".mvx-admin-object__name");
  await expect(names).toHaveText(["OPEX Planning Grid", "Headcount Grid"]);

  // The toolbar's tag chips filter; so does search on a tag.
  await page.locator(".mvx-toolbar").getByRole("button", { name: "people" }).click();
  await expect(names).toHaveText(["Headcount Grid"]);
  await page.locator(".mvx-toolbar").getByRole("button", { name: "people" }).click();
  await page.getByPlaceholder("Search grids, tags, metrics, dimensions…").fill("opex");
  await expect(names).toHaveText(["OPEX Planning Grid"]);
  await page.getByPlaceholder("Search grids, tags, metrics, dimensions…").fill("");

  // Add one tag, remove another.
  await page.getByRole("button", { name: "Edit grid OPEX Planning Grid" }).click();
  await page.getByRole("button", { name: "Remove tag plan" }).click();
  const tag = page.getByRole("textbox", { name: "Add tag" });
  await tag.fill("Board Pack");
  await tag.press("Enter");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => patched).toEqual({ name: "OPEX Planning Grid", tags: ["opex", "board-pack"] });
});

test("grids: a new grid is created with its tags", async ({ page }) => {
  await mockApi(page);
  let posted: unknown = null;
  await page.route((url) => url.pathname === "/api/developer/grids", async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    posted = route.request().postDataJSON();
    await route.fulfill({ json: { id: "g-new", status: "created" } });
  });
  await loadAs(page, "developer");
  await nav(page, "Grids").click();
  await page.getByRole("button", { name: "New grid" }).click();
  await page.getByPlaceholder("OPEX Budget").fill("Capex Grid");
  const tag = page.getByRole("textbox", { name: "Add tag" });
  await tag.fill("capex");
  await tag.press("Enter");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect.poll(() => posted).toMatchObject({ name: "Capex Grid", tags: ["capex"] });
});

test("dashboards: the edit button adds and removes tags", async ({ page }) => {
  await mockApi(page);
  let patched: unknown = null;
  await page.route((url) => url.pathname === "/api/developer/dashboards/dash-2", async (route) => {
    patched = route.request().postDataJSON();
    await route.fulfill({ json: { status: "ok" } });
  });
  await loadAs(page, "developer");
  await nav(page, "Dashboards").click();
  await page.getByRole("button", { name: "Edit dashboard OPEX Dashboard" }).click();
  await page.getByRole("button", { name: "Remove tag actuals" }).click();
  const tag = page.getByRole("textbox", { name: "Add tag" });
  await tag.fill("Monthly Close");
  await tag.press("Enter");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => patched).toEqual({ name: "OPEX Dashboard", tags: ["finance", "monthly-close"] });
});

test("users: search by name, e-mail or role", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await nav(page, "Users").click();
  const search = page.getByPlaceholder("Search name, e-mail, role, workspace…");
  const rows = page.locator("table.mvx-table tbody tr");
  const all = await rows.count();
  expect(all).toBeGreaterThan(1);
  await search.fill("alex@example");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("Alex Smith");
  await search.fill("no-such-person");
  await expect(page.getByText('No users match "no-such-person"')).toBeVisible();
});

test("sidebar: drag to resize, remembered, double-click resets", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  const sidebar = page.locator(".mvx-app-shell__sidebar");
  const width = async () => (await sidebar.boundingBox())!.width;
  const start = await width();
  const handle = page.getByRole("separator", { name: "Resize sidebar" });
  const box = (await handle.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + 200);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 100, box.y + 200, { steps: 5 });
  await page.mouse.up();
  expect(await width()).toBe(start + 100);

  await page.reload();
  await expect(handle).toBeVisible();
  expect(await width()).toBe(start + 100);

  await handle.dblclick();
  expect(await width()).toBe(start);

  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await expect(handle).toHaveCount(0);
  await page.getByRole("button", { name: "Expand sidebar" }).click();
});

test("business admin sidebar groups read User and Business Admin", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "finance");
  const labels = page.locator(".mvx-sidebar-nav__group-label");
  await expect(labels).toHaveText(["User", "Business Admin"], { useInnerText: false });
  // Forms have no screen of their own: records are worked on in a
  // dashboard's form widget.
  await expect(page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Forms" })).toHaveCount(0);
});
