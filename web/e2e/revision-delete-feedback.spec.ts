/**
 * Deleting a revision says what is happening (2026-10-13): a revision of
 * 618 metrics took minutes to delete, and Models showed nothing — no sign
 * it was running, and the failure when a reload cancelled it went nowhere.
 * The person clicked each trash icon three times and reported that delete
 * did nothing.
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const LIVE = { id: "rev-live", name: "Plan live", created_at: "2026-10-01T00:00:00Z" };
const DRAFT = { id: "rev-draft", name: "Plan draft", created_at: "2026-10-13T00:00:00Z" };

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

async function mockModel(page: Page, revisions: () => typeof LIVE[]) {
  await mockApi(page);
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json([{
    id: "t-1", name: "Acme", plan: "test", created_at: "2026-01-01T00:00:00Z",
    applications: [{
      id: "app-1", name: "Planning", mode: "planning", status: "active",
      models: [{ id: "model-1", name: "In Order Planning", storage_type: "oltp", active_revision: LIVE.name, is_default: true, revisions: revisions() }],
    }],
  }])));
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...LIVE, is_active: true }])));
}

async function deleteDraft(page: Page) {
  const model = page.locator(".mvx-admin-model", { hasText: "In Order Planning" });
  await expect(model).toBeVisible({ timeout: 15_000 });
  await model.getByRole("button", { name: `Delete revision ${DRAFT.name}` }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Delete revision" }).click();
  return model;
}

test("a slow revision delete shows it is running, then why it failed", async ({ page }) => {
  let answer: (() => void) | null = null;
  await mockModel(page, () => [LIVE, DRAFT]);
  await page.route("**/api/developer/revisions/**", (route) => {
    if (route.request().method() !== "DELETE") return route.fallback();
    return new Promise<void>((resolve) => {
      answer = () => resolve(route.fulfill(json({ error: "canceling statement due to user request" }, 500)));
    });
  });

  await loadAs(page, "developer");
  const model = await deleteDraft(page);

  const row = model.locator(".mvx-admin-revision", { hasText: DRAFT.name });
  await expect(row).toContainText("Deleting…");
  await expect(row).toHaveAttribute("aria-busy", "true");
  await expect(row.getByRole("button", { name: `Delete revision ${DRAFT.name}` })).toBeDisabled();

  await expect.poll(() => answer !== null).toBe(true);
  answer!();
  await expect(model.getByRole("alert")).toHaveText("Not deleted: 500: canceling statement due to user request");
  await expect(row).not.toContainText("Deleting…");
  await expect(row.getByRole("button", { name: `Delete revision ${DRAFT.name}` })).toBeEnabled();
});

test("a revision delete that succeeds removes the row", async ({ page }) => {
  let deleted = false;
  await mockModel(page, () => (deleted ? [LIVE] : [LIVE, DRAFT]));
  await page.route("**/api/developer/revisions/**", (route) => {
    if (route.request().method() !== "DELETE") return route.fallback();
    deleted = true;
    return route.fulfill(json({ status: "deleted" }));
  });

  await loadAs(page, "developer");
  const model = await deleteDraft(page);

  await expect(model.locator(".mvx-admin-revision", { hasText: DRAFT.name })).toHaveCount(0);
  await expect(model.getByRole("alert")).toHaveCount(0);
});

// A revision removed since the list was read (a discarded AI draft) answers
// 404: it is gone, as asked. The row used to stay beside "Not deleted: 404:
// revision not found" (reported 2026-10-29).
test("a revision delete answered 404 reads the list again and removes the row", async ({ page }) => {
  let removedElsewhere = false;
  await mockModel(page, () => (removedElsewhere ? [LIVE] : [LIVE, DRAFT]));
  await page.route("**/api/developer/revisions/**", (route) => {
    if (route.request().method() !== "DELETE") return route.fallback();
    removedElsewhere = true;
    return route.fulfill(json({ error: "revision not found" }, 404));
  });

  await loadAs(page, "developer");
  const model = await deleteDraft(page);

  await expect(model.locator(".mvx-admin-revision", { hasText: DRAFT.name })).toHaveCount(0);
  await expect(model.getByRole("alert")).toHaveCount(0);
});
