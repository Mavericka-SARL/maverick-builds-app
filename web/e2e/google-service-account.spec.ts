/**
 * Integrations › Google Sheets › the tenant's Google service account for
 * private sheets: a collapsed panel under the Sheets section, opened by the
 * developer who imports; saving sends the key file and the panel then shows
 * the address to share sheets with.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("developer stores a service-account key file under Google Sheets and sees the address to share with", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/integrations/google-service-account", async (route) => {
    if (route.request().method() === "PUT") {
      const body = route.request().postDataJSON() as { key_file: string };
      expect(body.key_file).toContain("service_account");
      await route.fulfill({ json: { configured: true, client_email: "sheets@acme.iam.gserviceaccount.com", project_id: "acme" } });
      return;
    }
    await route.fallback();
  });
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: /Google Sheets/ }).click();
  const panel = page.getByTestId("google-service-account");
  await expect(panel.getByTestId("google-service-account-summary")).toContainText("link-shared sheets only");
  await panel.getByRole("button", { name: /Private sheets/ }).click();
  await expect(panel).toContainText("only link-shared sheets");
  await page.getByLabel("Google service account key file").fill('{"type":"service_account","client_email":"sheets@acme.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\\nx\\n-----END PRIVATE KEY-----\\n"}');
  await page.getByRole("button", { name: "Save key" }).click();
  await expect(page.getByTestId("google-sa-email")).toHaveText("sheets@acme.iam.gserviceaccount.com");
  await expect(panel.getByTestId("google-service-account-summary")).toContainText("share sheets with sheets@acme.iam.gserviceaccount.com");
});

test("there is no separate Connections tab any more", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await expect(page.getByRole("button", { name: "Connections", exact: true })).toHaveCount(0);
  await loadAs(page, "tenant_admin");
  await expect(page.getByRole("button", { name: "Connections", exact: true })).toHaveCount(0);
});
