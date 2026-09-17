/**
 * The edition a deployment runs is deployment-wide and must be visible to
 * every role (account menu) and explained to the platform admin (Platform ›
 * License): which key is in force, what it unlocks, what is locked and why.
 * Without a key the console must still render — as Community.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, enterpriseLicense } from "./mocks";

const account = (page: import("@playwright/test").Page) => page.getByRole("button", { name: /^Account:/ });

test("community: the account menu names the edition and License shows locked features", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Community edition/);
  await page.keyboard.press("Escape");

  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await expect(tab).toContainText("Community edition");
  await expect(tab).toContainText("No license key");
  await expect(tab.getByTestId("feature-sso")).toContainText("Locked");
  await expect(tab.getByTestId("feature-white_label")).toContainText("Commercial");
  await expect(tab).toContainText("MAVERICKS_LICENSE_KEY");
});

test("enterprise: the key's details and unlocked features are reported", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await expect(tab).toContainText("Enterprise edition");
  await expect(tab).toContainText("Acme Corp");
  await expect(tab).toContainText("max_users = 50");
  await expect(tab.getByTestId("feature-sso")).toContainText("Included");
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Enterprise edition/);
});

test("expired key: runs as community and says so", async ({ page }) => {
  await mockApi(page, { license: { ...enterpriseLicense, edition: "community", state: "expired", features: [], expires_at: "2026-01-01T00:00:00Z" } });
  await loadAs(page, "developer");
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Community edition · license expired/);
});

test("a developer (no admin section) still sees the edition, and no License tab", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await expect(page.getByRole("button", { name: "License", exact: true })).toHaveCount(0);
  await account(page).click();
  await expect(page.getByTestId("edition")).toBeVisible();
});
