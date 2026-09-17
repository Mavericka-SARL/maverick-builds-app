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

test("Notification bell renders in all 4 consoles", async ({ page }) => {
  await mockApi(page);
  for (const persona of ["dept_head", "finance", "developer", "platform_admin"]) {
    await loadAs(page, persona);
    // The bell, specifically: a sidebar item may also be called Notifications.
    await expect(page.locator('button[aria-label^="Notifications"]')).toBeVisible({ timeout: 15_000 });
  }
});
