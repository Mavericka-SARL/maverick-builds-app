/**
 * The dashboard designer keeps every change as a draft until Save — moving,
 * resizing and editing a widget, and also adding, duplicating and deleting
 * one. Cancel drops them all; Save writes them: the new widgets created, the
 * deleted ones removed, the rest updated.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs, acceptLiveEdits } from "./mocks";

const REV = { id: "rev-live", name: "Live", created_at: "2026-08-01T00:00:00Z" };
const dashboards = [{
  id: "dash-1", name: "Draft Dash", tags: [], folder_id: null,
  widgets: [{
    id: "w-1", widget_type: "text", ref_id: null, content: "EXISTING-WIDGET", title: null, show_title: false,
    widget_props: null, sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 300, size_h: 100,
  }],
}];

type Sent = { method: string; path: string; body: unknown };

async function openDesigner(page: Page): Promise<Sent[]> {
  const sent: Sent[] = [];
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...REV, is_active: true }])));
  await acceptLiveEdits(page, REV.id);
  await page.route("**/api/developer/folders*", (route) => route.fulfill(json([])));
  await page.route((url) => url.pathname.startsWith("/api/developer/dashboards"), (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    if (req.method() !== "GET") {
      sent.push({ method: req.method(), path, body: req.postDataJSON() });
      return route.fulfill(json(req.method() === "POST" ? { id: `created-${sent.length}` } : { status: "ok" }));
    }
    return route.fulfill(json(dashboards));
  });
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Dashboards", exact: true }).click();
  const row = page.getByRole("button", { name: "Draft Dash", exact: true }).locator("xpath=ancestor::div[contains(@class,'mvx-admin-object')][1]");
  await row.getByRole("button", { name: "Design", exact: true }).click();
  await expect(page.getByText("EXISTING-WIDGET").first()).toBeVisible({ timeout: 15_000 });
  return sent;
}

async function addText(page: Page, text: string) {
  await page.getByRole("button", { name: "Text", exact: true }).click();
  await page.getByPlaceholder("Text content…").fill(text);
  await page.getByRole("button", { name: "Add widget" }).click();
  await expect(page.getByText(text).first()).toBeVisible();
}

test("an added widget waits for Save, and Cancel removes it", async ({ page }) => {
  const sent = await openDesigner(page);
  await addText(page, "NEW-WIDGET");
  await expect(page.getByText("Unsaved changes")).toBeVisible();
  expect(sent).toEqual([]);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByText("NEW-WIDGET")).toHaveCount(0);
  await expect(page.getByText("Unsaved changes")).toHaveCount(0);
  expect(sent).toEqual([]);
});

test("Save creates the added widgets and deletes the removed ones", async ({ page }) => {
  const sent = await openDesigner(page);
  await addText(page, "SAVED-WIDGET");
  // Remove the existing widget: still on the server until Save.
  await page.locator(".mvx-canvas-widget").filter({ hasText: "EXISTING-WIDGET" }).getByRole("button", { name: "Delete widget" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Delete" }).click();
  await expect(page.getByText("EXISTING-WIDGET")).toHaveCount(0);
  expect(sent).toEqual([]);

  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => sent.length).toBe(2);
  expect(sent[0]).toMatchObject({ method: "POST", path: "/api/developer/dashboards/dash-1/widgets", body: { widget_type: "text", content: "SAVED-WIDGET" } });
  expect(sent[1]).toMatchObject({ method: "DELETE", path: "/api/developer/dashboards/dash-1/widgets/w-1" });
});
