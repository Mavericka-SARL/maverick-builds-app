/**
 * Design mode must show the widgets of a dashboard that lives in a
 * NON-active revision. The Dashboards tab lists dashboards for the
 * developer's selected revision, but the canvas used to re-fetch the list
 * without a revision — the server then answers with the ACTIVE revision's
 * dashboards, the one being designed is not among them, and the canvas
 * opened empty ("Sales Overview", 6 widgets, blank design mode — reported
 * live, 2026-09-10).
 *
 * Self-contained: the API is mocked (CI runs the e2e job without a
 * gateway). The mock reproduces the server's rule exactly — a dashboard
 * list is answered for the revision asked for, and a request WITHOUT
 * revision_id gets the active revision's (empty) list.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const ACTIVE_REV = { id: "rev-live", name: "Live", created_at: "2026-08-01T00:00:00Z" };
const DRAFT_REV = { id: "rev-draft", name: "Design Mode Test", created_at: "2026-09-01T00:00:00Z" };
const DASH_NAME = "Design Mode Dash";
const WIDGET_TEXT = "DESIGN-MODE-WIDGET";
const FAR_RIGHT_TEXT = "FAR-RIGHT-WIDGET";

const draftDashboards = [{
  id: "dash-draft",
  name: DASH_NAME,
  tags: [],
  folder_id: null,
  widgets: [{
    // Sized in pixels, like the canvas saves them: prose is laid out in the
    // widget's own box, so a widget a few pixels wide has nowhere to put it.
    id: "w-1", widget_type: "text", ref_id: null, content: WIDGET_TEXT, title: null, show_title: false,
    widget_props: null, sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 300, size_h: 60,
  }, {
    // A grid widget: design mode must show ITS selectors (grid-1's dims minus
    // the first, which lands on columns by default) as a strip on the card.
    id: "w-2", widget_type: "grid", ref_id: "grid-1", content: null, title: null, show_title: false,
    widget_props: { sync_context: true }, sort_order: 1, col_start: 1, col_span: 12, pos_x: 0, pos_y: 200, size_w: 6, size_h: 3,
  }, {
    // A second synced grid: in Preview it FOLLOWS w-2's shared selector, and
    // design mode must say so instead of listing the selector again.
    id: "w-3", widget_type: "grid", ref_id: "grid-1", content: null, title: "Second grid", show_title: true,
    widget_props: { sync_context: true }, sort_order: 2, col_start: 1, col_span: 12, pos_x: 0, pos_y: 400, size_w: 6, size_h: 3,
  }, {
    // Placed beyond 1200px on the design canvas. Preview used to clamp the
    // dashboard to a fixed 1200px / 60 columns, so a widget designed out
    // here was squeezed or cut off (a fourth KPI clipped, reported live
    // 2026-09-11) — Preview must lay widgets out at the designed x/width.
    id: "w-4", widget_type: "text", ref_id: null, content: FAR_RIGHT_TEXT, title: null, show_title: false,
    widget_props: null, sort_order: 3, col_start: 1, col_span: 12, pos_x: 1200, pos_y: 0, size_w: 300, size_h: 60,
  }],
}];

test("design mode shows the widgets of a dashboard in a non-active revision", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  // Later registrations win, so these override the catch-all above.
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json([{
    id: "t-1", name: "Acme", plan: "enterprise", created_at: "2026-01-01T00:00:00Z",
    applications: [{
      id: "app-1", name: "Planning", mode: "planning", status: "active",
      models: [{ id: "model-1", name: "Finance Model", storage_type: "oltp", active_revision: ACTIVE_REV.name, is_default: true, revisions: [ACTIVE_REV, DRAFT_REV] }],
    }],
  }])));
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([
    { ...ACTIVE_REV, is_active: true }, { ...DRAFT_REV, is_active: false },
  ])));
  await page.route("**/api/developer/folders*", (route) => route.fulfill(json([])));
  await page.route("**/api/developer/dashboards*", (route) => {
    const rev = new URL(route.request().url()).searchParams.get("revision_id");
    // The server's rule: no revision_id → the ACTIVE revision, which has no such dashboard.
    return route.fulfill(json(rev === DRAFT_REV.id ? draftDashboards : []));
  });

  await loadAs(page, "developer");
  // Models tab (the developer landing): pick the non-active revision.
  const draftRow = page.locator(".mvx-admin-revision", { hasText: DRAFT_REV.name }).first();
  await draftRow.click();
  await expect(draftRow.getByText("Working")).toBeVisible({ timeout: 15_000 });

  await page.getByRole("button", { name: "Dashboards", exact: true }).click();
  // The dashboard's own card is the nearest .mvx-admin-object around its
  // name button (folders wrap their dashboards in the same class).
  const row = page.getByRole("button", { name: DASH_NAME, exact: true }).locator("xpath=ancestor::div[contains(@class,'mvx-admin-object')][1]");
  await expect(row).toBeVisible({ timeout: 15_000 });
  await expect(row.getByText("4 widgets")).toBeVisible();
  // No folders in this revision: no "Unfiled" group header, no folder
  // filter and no per-row "move to folder" select whose only choice would
  // be "Unfiled" — the list is flat (see the folder test below).
  await expect(page.getByText("Unfiled", { exact: true })).toHaveCount(0);
  await expect(page.getByLabel("Filter by folder")).toHaveCount(0);
  await expect(page.getByLabel(`Folder for ${DASH_NAME}`)).toHaveCount(0);
  await row.getByRole("button", { name: "Design", exact: true }).click();

  await expect(page.getByText(WIDGET_TEXT).first()).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("No widgets on this dashboard yet.")).toHaveCount(0);

  // Design mode shows each widget's own selectors (preview dedupes shared
  // ones onto the first widget, hiding which widget carries which).
  const strip = page.locator('[aria-label="Widget selectors"]');
  await expect(strip).toHaveCount(2);
  await expect(strip.first()).toContainText("synced");
  // One shared selector: the first synced widget draws it, the second follows.
  await expect(strip.nth(1)).toContainText("→");
  await expect(strip.first()).not.toContainText("→");

  // The developer chooses where a widget's selectors sit; the strip says so.
  await page.locator(".mvx-canvas-widget").filter({ hasText: "GRID" }).first().click();
  const pos = page.getByLabel("Selectors position");
  await expect(pos).toBeVisible();
  await pos.selectOption("bottom");
  await expect(strip.first()).toContainText("bottom");

  // Background: every widget type offers white card / no background; a bare
  // type (grid) defaults to none.
  const bg = page.getByLabel("Widget background");
  await expect(bg).toHaveValue("none");
  await bg.selectOption("white");

  // Preview renders in the revision being designed, not the active one.
  const gridRequests: string[] = [];
  page.on("request", (r) => { if (r.url().includes("/api/grid?")) gridRequests.push(r.url()); });
  await page.getByRole("radio", { name: "Preview" }).or(page.getByRole("button", { name: "Preview", exact: true })).first().click();
  await expect.poll(() => gridRequests.length, { timeout: 10_000 }).toBeGreaterThan(0);
  expect(gridRequests.every((u) => u.includes(`revision_id=${DRAFT_REV.id}`))).toBe(true);

  // Preview keeps the designed geometry: the widget placed at x=1200 with
  // width 300 sits exactly there, 1200px right of the first column.
  const cells = page.locator(".mvx-dash-cell");
  await expect(cells).toHaveCount(4);
  const firstBox = (await cells.first().boundingBox())!;
  const farRight = cells.filter({ hasText: FAR_RIGHT_TEXT });
  await expect(farRight).toHaveCount(1);
  const farBox = (await farRight.boundingBox())!;
  expect(Math.round(farBox.x - firstBox.x)).toBe(1200);
  expect(Math.round(farBox.width)).toBe(300);
  // The unsaved "white card" choice shows in Preview on that grid widget only.
  await expect(page.locator(".mvx-dash-cell--card")).toHaveCount(1);
});

test("folder controls appear once the revision has a folder", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/developer/folders*", (route) => route.fulfill(json([{ id: "f-1", name: "Finance", parent_id: null }])));
  await page.route("**/api/developer/dashboards*", (route) => route.fulfill(json(draftDashboards)));
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Dashboards", exact: true }).click();
  await expect(page.getByRole("button", { name: DASH_NAME, exact: true })).toBeVisible({ timeout: 15_000 });
  // With a folder to move into, the grouping and the controls are useful.
  // (getByText would also count the "Unfiled" <option>s of the two selects.)
  await expect(page.locator("span", { hasText: /^Unfiled$/ })).toHaveCount(1);
  await expect(page.locator("span", { hasText: /^Finance$/ })).toBeVisible();
  await expect(page.getByLabel("Filter by folder")).toBeVisible();
  await expect(page.getByLabel(`Folder for ${DASH_NAME}`)).toBeVisible();
});
