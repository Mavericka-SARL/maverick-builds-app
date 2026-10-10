/**
 * Applications: each level of the tree says what it is — Tenant,
 * Application, Model — with a one-line explanation on hover, so "Getting
 * started" reads as an application and "Learn the platform" as a model.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("the applications tree labels tenants, applications and models", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "tenant_admin");
  await expect(page.getByRole("heading", { name: "Applications" })).toBeVisible({ timeout: 15_000 });

  const tag = (kind: string) => page.locator(".mvx-kind-tag", { hasText: new RegExp(`^${kind}$`, "i") });
  await expect(tag("Tenant").first()).toBeVisible();
  await expect(tag("Application").first()).toBeVisible();
  await expect(tag("Model").first()).toBeVisible();
  await expect(tag("Model").first()).toHaveAttribute("title", /It holds revisions/);
  await expect(page.locator(".mvx-admin-model__name", { hasText: "Finance Model" }).locator(".mvx-kind-tag")).toHaveText(/model/i);
  await expect(page.locator(".mvx-admin-revisions__label").first()).toHaveAttribute("title", /active revision is the one everyone works in/);
  await page.screenshot({ path: test.info().outputPath("tenancy-kind-tags.png"), fullPage: true });
});
