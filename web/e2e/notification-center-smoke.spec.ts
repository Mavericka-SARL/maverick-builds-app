/**
 * Smoke test: the notification bell (web/src/ui/NotificationCenter.tsx)
 * shows an unread-count badge, the drawer lists notifications from the
 * existing GET /api/notifications API, clicking an unread one calls
 * POST /api/notifications/mark-read and — for a workflow_instance-linked
 * notification — navigates to the History tab with the right instance
 * visible. Closes the P1 "notification center" backlog item: the REST API
 * and DB schema already existed; this is the first frontend consumer of
 * either.
 *
 * Uses the same purpose-built mock as the other specs in this directory
 * (no live Go gateway in the e2e CI job — see business-console-grid-smoke's
 * header comment for why).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

async function spyOnMarkRead(page: Page): Promise<{ bodies: unknown[] }> {
  const spy = { bodies: [] as unknown[] };
  await page.route("**/api/notifications/mark-read", async (route) => {
    spy.bodies.push(route.request().postDataJSON());
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok" }) });
  });
  return spy;
}

test("Notification bell: unread badge, drawer list, mark-read, navigate to history", async ({ page }) => {
  await mockApi(page);
  const markReadSpy = await spyOnMarkRead(page);
  await loadAs(page, "dept_head");

  // Unread-count badge: one of the two mocked notifications is "delivered"
  // (unread), the other "read".
  const bell = page.getByRole("button", { name: /Notifications, 1 unread/i });
  await expect(bell).toBeVisible({ timeout: 15_000 });
  // Badge is a sibling of the button (absolutely positioned over its
  // corner), not a descendant; "danger" color is unique to the unread
  // count badge among the other mvx-badge elements on screen.
  await expect(page.locator(".mvx-badge--danger")).toHaveText("1");

  await bell.click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByText("Budget Approval needs your review")).toBeVisible();
  await expect(page.getByText("Sales Pipeline dashboard updated")).toBeVisible();

  // Click the unread, workflow_instance-linked notification.
  await page.getByText("Budget Approval needs your review").click();

  // Marks it read.
  await expect.poll(() => markReadSpy.bodies.length).toBeGreaterThan(0);
  expect(markReadSpy.bodies[0]).toEqual({ ids: ["notif-1"] });

  // Navigates to History and the linked instance (inst-1, "Budget Approval"
  // in mocks.ts's `history` fixture) is visible.
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await expect(page.locator("#wf-history-inst-1")).toBeVisible();
});

test("Notifications can be deleted one at a time or all at once, the latter after asking", async ({ page }) => {
  await mockApi(page);
  const deleted: string[] = [];
  let list: { id: string; template_id: string; status: string; created_at: string; template_vars: Record<string, string> }[] = [
    { id: "n-1", template_id: "t", status: "delivered", created_at: "2026-11-01T10:00:00Z", template_vars: { subject: "First notice" } },
    { id: "n-2", template_id: "t", status: "read", created_at: "2026-11-01T09:00:00Z", template_vars: { subject: "Second notice" } },
    { id: "n-3", template_id: "t", status: "read", created_at: "2026-11-01T08:00:00Z", template_vars: { subject: "Third notice" } },
  ];
  await page.route("**/api/notifications", async (route) => {
    if (route.request().method() === "DELETE") {
      deleted.push("all");
      list = [];
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ deleted: 2 }) });
    }
    return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(list) });
  });
  await page.route("**/api/notifications/n-*", async (route) => {
    const id = new URL(route.request().url()).pathname.split("/").pop() ?? "";
    deleted.push(id);
    list = list.filter((n) => n.id !== id);
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ deleted: 1 }) });
  });
  await loadAs(page, "dept_head");
  await page.locator('button[aria-label^="Notifications"]').click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.getByText("First notice")).toBeVisible();

  // The bin deletes that one, and does not open or mark it.
  await drawer.getByRole("button", { name: "Delete notification: First notice" }).click();
  await expect(drawer.getByText("First notice")).toHaveCount(0);
  expect(deleted).toEqual(["n-1"]);
  await expect(drawer.getByText("Second notice")).toBeVisible();

  // Delete all asks first: cancelling changes nothing.
  await drawer.getByRole("button", { name: "Delete all" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel" }).click();
  expect(deleted).toEqual(["n-1"]);
  await expect(drawer.getByText("Second notice")).toBeVisible();

  await drawer.getByRole("button", { name: "Delete all" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Delete all" }).click();
  await expect(drawer.getByText("No notifications yet.")).toBeVisible();
  expect(deleted).toEqual(["n-1", "all"]);
  // Nothing left to delete.
  await expect(drawer.getByRole("button", { name: "Delete all" })).toHaveCount(0);
});

test("Notification bell renders in all 4 consoles", async ({ page }) => {
  await mockApi(page);
  for (const persona of ["dept_head", "finance", "developer", "platform_admin"]) {
    await loadAs(page, persona);
    // The bell, specifically: a sidebar item may also be called Notifications.
    await expect(page.locator('button[aria-label^="Notifications"]')).toBeVisible({ timeout: 15_000 });
  }
});
