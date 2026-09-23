/**
 * Admin › Single sign-on and Admin › Provisioning (SCIM) are enterprise
 * screens: gated on other editions, real forms with an enterprise key. The
 * SSO tab must show the tenant what to register at their provider; the SCIM
 * tab must show an issued token exactly once and let it be revoked.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, chooseTenant, enterpriseLicense, ssoSettings } from "./mocks";

const tab = (page: import("@playwright/test").Page, name: string) =>
  page.getByRole("button", { name, exact: true }).click();

test("community: both tabs render the feature gate", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await tab(page, "Single sign-on");
  await chooseTenant(page);
  await expect(page.locator(".mvx-feature-gate")).toContainText("Single sign-on");
  await expect(page.locator(".mvx-feature-gate")).toContainText("requires the Enterprise edition");
  await expect(page.getByTestId("sso-settings")).toHaveCount(0);
  await tab(page, "Provisioning (SCIM)");
  await chooseTenant(page);
  await expect(page.locator(".mvx-feature-gate")).toContainText("SCIM provisioning");
  await expect(page.getByTestId("scim-tokens")).toHaveCount(0);
});

test("enterprise: the SSO form shows the broker endpoint and adapts to the protocol", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await loadAs(page, "platform_admin");
  await tab(page, "Single sign-on");
  await chooseTenant(page);
  const panel = page.getByTestId("sso-settings");
  await expect(panel).toBeVisible();
  await expect(panel.getByTestId("broker-endpoint")).toHaveText(/\/broker\/mvx-0000\/endpoint$/);
  await expect(panel.getByLabel("OIDC client id")).toBeVisible();
  await expect(panel.getByRole("button", { name: "Register provider" })).toBeVisible();
  await expect(panel.getByRole("button", { name: "Remove provider" })).toHaveCount(0);

  await panel.getByLabel("SSO protocol").selectOption("saml");
  await expect(panel.getByLabel("OIDC client id")).toHaveCount(0);
  await expect(panel).toContainText("Assertion consumer service URL");
  await expect(panel).toContainText("Service provider entity id");
});

test("enterprise with a registered provider: remove asks first", async ({ page }) => {
  await mockApi(page, {
    license: enterpriseLicense,
    sso: { ...ssoSettings, alias: "mvx-0000", configured: true, enabled: true, allowed_domains: ["acme.test"], metadata_url: "https://idp.acme.test/.well-known/openid-configuration", client_id: "mavericks" },
  });
  await loadAs(page, "platform_admin");
  await tab(page, "Single sign-on");
  await chooseTenant(page);
  const panel = page.getByTestId("sso-settings");
  await expect(panel.getByLabel("Allowed e-mail domains")).toHaveValue("acme.test");
  await expect(panel.getByLabel("OIDC client secret")).toHaveAttribute("placeholder", "••••••••");
  await panel.getByRole("button", { name: "Remove provider" }).click();
  await expect(page.getByText("Accounts it created stay")).toBeVisible();
});

test("enterprise: SCIM tokens list, issue once, revoke asks", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await loadAs(page, "platform_admin");
  await tab(page, "Provisioning (SCIM)");
  await chooseTenant(page);
  const panel = page.getByTestId("scim-tokens");
  await expect(panel.getByTestId("scim-token-tok-1")).toContainText("Active");
  await expect(panel.getByTestId("scim-token-tok-2")).toContainText("Revoked");
  await expect(panel.getByTestId("scim-token-tok-2").getByRole("button", { name: "Revoke" })).toHaveCount(0);

  await expect(panel.getByRole("button", { name: "Issue token" })).toBeDisabled();
  await panel.getByLabel("Token name").fill("New directory");
  await panel.getByRole("button", { name: "Issue token" }).click();
  await expect(panel.getByTestId("scim-token")).toContainText("mvx_scim_");
  await expect(panel.getByTestId("scim-base-url")).toHaveText("https://console.test/api/scim/v2");
  await expect(panel).toContainText("not shown again");

  await panel.getByTestId("scim-token-tok-1").getByRole("button", { name: "Revoke" }).click();
  await expect(page.getByText("stops being able to provision")).toBeVisible();
});

test("a developer sees neither tab", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await loadAs(page, "developer");
  await expect(page.getByRole("button", { name: "Single sign-on", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Provisioning (SCIM)", exact: true })).toHaveCount(0);
});
