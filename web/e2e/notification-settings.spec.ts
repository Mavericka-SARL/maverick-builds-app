/**
 * Admin › Notifications is where a tenant turns outbound delivery on. Every
 * notification always reaches the console; these switches decide whether it
 * also leaves the platform, so the screen has to be explicit about what is
 * off, and honest when the deployment cannot send mail at all.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("the settings screen explains each channel and warns when no relay exists", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Notification delivery", exact: true }).click();

  const panel = page.getByTestId("notification-settings");
  await expect(panel).toContainText("no mail relay configured");
  await expect(panel.getByRole("checkbox", { name: "Send notifications by e-mail" })).not.toBeChecked();
  await expect(panel.getByRole("checkbox", { name: "Send notifications to a webhook" })).not.toBeChecked();
  await expect(panel.getByRole("checkbox", { name: "Remind assignees about tasks that come due" })).not.toBeChecked();

  // The webhook fields only matter once the channel is on.
  await expect(panel.getByLabel("Webhook URL")).toHaveCount(0);
  await panel.getByRole("checkbox", { name: "Send notifications to a webhook" }).check();
  await expect(panel.getByLabel("Webhook URL")).toBeVisible();
  await expect(panel.getByLabel("Webhook signing secret")).toBeVisible();

  // Same for the reminder lead time.
  await expect(panel.getByLabel("Reminder lead hours")).toHaveCount(0);
  await panel.getByRole("checkbox", { name: "Remind assignees about tasks that come due" }).check();
  await expect(panel.getByLabel("Reminder lead hours")).toBeVisible();

  await expect(page.getByRole("button", { name: "Save settings" })).toBeEnabled();
});

test("a developer has no notification settings tab", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await expect(page.getByRole("button", { name: "Notification delivery", exact: true })).toHaveCount(0);
});
