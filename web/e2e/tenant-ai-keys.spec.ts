/**
 * Admin › AI keys is the first enterprise-gated screen. On a community
 * deployment the tab must still be there — that is how an admin learns the
 * option exists — but it must render the feature gate rather than a form that
 * would 403 on save. With an enterprise key it must render the real form, and
 * must never let an admin enforce a key that is not stored.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, enterpriseLicense, tenantAISettings } from "./mocks";

const openTab = async (page: import("@playwright/test").Page) =>
  page.getByRole("button", { name: "AI keys", exact: true }).click();

test("community: the tab explains which edition unlocks it, and shows no form", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await openTab(page);
  const gate = page.locator(".mvx-feature-gate");
  await expect(gate).toContainText("Tenant AI keys");
  await expect(gate).toContainText("requires the Enterprise edition");
  await expect(gate).toContainText("this deployment runs the Community edition");
  await expect(page.getByTestId("tenant-ai-settings")).toHaveCount(0);
});

test("enterprise: the form renders and enforcing is blocked until a key is stored", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await loadAs(page, "platform_admin");
  await openTab(page);

  const panel = page.getByTestId("tenant-ai-settings");
  await expect(panel).toBeVisible();
  await expect(page.locator(".mvx-feature-gate")).toHaveCount(0);

  // No key stored: the enforce switch is inert and says why.
  const enforce = panel.getByLabel(/Use the tenant key for every AI call/);
  await expect(enforce).toBeDisabled();
  await expect(panel).toContainText("Store a key first");
  // Nothing to remove yet.
  await expect(panel.getByRole("button", { name: "Remove key" })).toHaveCount(0);

  // Typing a key is enough to allow enforcing, before it is even saved.
  await panel.getByLabel("Tenant API key").fill("sk-test-key");
  await expect(enforce).toBeEnabled();
  await enforce.check();
  await expect(panel).not.toContainText("Store a key first");
});

test("enterprise with a stored key: the key is masked and can be removed", async ({ page }) => {
  await mockApi(page, {
    license: enterpriseLicense,
    tenantAI: { ...tenantAISettings, provider: "anthropic", model: "claude-opus-4-8", has_key: true, enforced: true },
  });
  await loadAs(page, "platform_admin");
  await openTab(page);

  const panel = page.getByTestId("tenant-ai-settings");
  const key = panel.getByLabel("Tenant API key");
  await expect(key).toHaveAttribute("type", "password");
  await expect(key).toHaveValue("");
  await expect(key).toHaveAttribute("placeholder", "••••••••");
  await expect(panel.getByLabel(/Use the tenant key for every AI call/)).toBeChecked();
  await expect(panel.getByRole("button", { name: "Remove key" })).toBeVisible();

  // Removing is destructive, so it asks first and says what developers lose.
  await panel.getByRole("button", { name: "Remove key" }).click();
  await expect(page.getByText("Developers fall back to their own keys")).toBeVisible();
});

test("a developer never sees the tab", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await loadAs(page, "developer");
  await expect(page.getByRole("button", { name: "AI keys", exact: true })).toHaveCount(0);
});
