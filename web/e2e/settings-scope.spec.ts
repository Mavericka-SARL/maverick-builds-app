/**
 * Per-tenant settings at platform scope (migration 091): the platform admin
 * has no tenant of their own, so the settings tabs ask whose settings they
 * mean — the deployment's own row, or one tenant's — and address every
 * call accordingly. A tenant admin is never asked: the server knows theirs.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("platform admin: delivery settings open on the deployment's defaults, and a tenant can be chosen", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Notification delivery", exact: true }).click();
  const picker = page.getByLabel("Settings scope");
  await expect(picker).toHaveValue("");
  await expect(page.getByTestId("notification-settings")).toContainText("deployment's defaults");

  // Choosing a tenant re-reads its settings with the tenant on the request.
  const scoped = page.waitForRequest((r) => r.url().endsWith("/api/notifications/settings") && r.headers()["x-tenant-id"] === "tenant-1");
  await picker.selectOption("tenant-1");
  await scoped;
  await expect(page.getByTestId("notification-settings")).toContainText("follows the deployment's defaults");
});

test("identity settings wait for a tenant; a tenant admin is never asked", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "Single sign-on", exact: true }).click();
  await expect(page.getByTestId("settings-scope")).toContainText("Choose a tenant");
  await expect(page.getByTestId("sso-settings")).toHaveCount(0);

  await loadAs(page, "tenant_admin");
  await page.getByRole("button", { name: "Notification delivery", exact: true }).click();
  await expect(page.getByTestId("notification-settings")).toBeVisible();
  await expect(page.getByLabel("Settings scope")).toHaveCount(0);
});
