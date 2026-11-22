/**
 * The edition a deployment runs is deployment-wide and must be visible to
 * every role (account menu) and explained to the platform admin (Platform ›
 * License): which key is in force, what it unlocks, what is locked and why.
 * Without a key the console must still render — as Community.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, enterpriseLicense, chooseTenant } from "./mocks";

const account = (page: import("@playwright/test").Page) => page.getByRole("button", { name: /^Account:/ });

test("community: the account menu names the edition and License shows locked features", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Community Edition/);
  await page.keyboard.press("Escape");

  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await expect(tab).toContainText("Community Edition");
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
  await expect(tab).toContainText("Enterprise Edition");
  await expect(tab).toContainText("Acme Corp");
  await expect(tab).toContainText("ORD-E2E-1");
  await expect(tab).toContainText("PFA 2026-10");
  await expect(tab).toContainText("2026-10");
  await expect(tab.getByTestId("feature-sso")).toContainText("Included");
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Enterprise Edition/);
});

test("expired key: runs as community and says so", async ({ page }) => {
  await mockApi(page, { license: { ...enterpriseLicense, edition: "community", state: "expired", features: [], expires_at: "2026-01-01T00:00:00Z" } });
  await loadAs(page, "developer");
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Community Edition · license expired/);
});

test("renewal due: the account menu says when the key expires", async ({ page }) => {
  await mockApi(page, { license: { ...enterpriseLicense, renewal_due: true, expires_at: "2027-09-01T00:00:00Z" } });
  await loadAs(page, "platform_admin");
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Enterprise Edition · license expires/);
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "License", exact: true }).click();
  await expect(page.getByTestId("license-tab")).toContainText("Install the renewed key before then");
});

// No transition period (owner, 2026-11-22): the day after its expiry date
// a key unlocks nothing, and every paid screen shows the edition note as it
// does without a key.
test("expired: the paid features are locked at once, as without a key", async ({ page }) => {
  await mockApi(page, { license: { ...enterpriseLicense, edition: "community", state: "expired", features: [], expires_at: "2026-11-01T23:59:59Z" } });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await expect(tab).toContainText("The deployment and every tenant run the Community Edition until a new key is installed");
  await expect(tab).not.toContainText(/transition/i);
  await expect(tab.getByTestId("feature-sso")).toContainText("Locked");
  await page.getByRole("button", { name: "Single sign-on", exact: true }).click();
  await chooseTenant(page);
  await expect(page.locator(".mvx-feature-gate")).toContainText("requires the Enterprise Edition");
  await expect(page.getByTestId("feature-transition")).toHaveCount(0);
});

test("a developer (no admin section) still sees the edition, and no License tab", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  await expect(page.getByRole("button", { name: "License", exact: true })).toHaveCount(0);
  await account(page).click();
  await expect(page.getByTestId("edition")).toBeVisible();
});

// A key runs to the end of its expiry day, UTC (cmd/license sign -expires
// 2026-11-23 → 2026-11-23T23:59:59Z). The console says that day wherever
// the viewer is: east of Greenwich the local date is already the next one
// (found 2026-11-21 in the Commercial key run, which read 11/24 for a key
// ordered to 11/23).
test.describe("in Luxembourg's time zone", () => {
  test.use({ timezoneId: "Europe/Luxembourg", locale: "en-US" });
  test("the expiry date is the order's day, not the next", async ({ page }) => {
    await mockApi(page, { license: { ...enterpriseLicense, renewal_due: true, expires_at: "2026-11-23T23:59:59Z" } });
    await loadAs(page, "platform_admin");
    await account(page).click();
    await expect(page.getByTestId("edition")).toContainText("expires 11/23/2026");
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: "License", exact: true }).click();
    await expect(page.getByTestId("license-tab")).toContainText("11/23/2026");
    await expect(page.getByTestId("license-tab")).not.toContainText("11/24/2026");
  });
});

// A platform administrator installs a key on the License page (owner,
// 2026-11-22): pasted, sent to the gateway, in force at once — the page and
// the account menu say the new edition without a reload. One installed here
// can be removed here; one set in the server's configuration cannot, and
// the page says where it is set instead of offering the field.
test("community: a platform admin pastes a key and the edition changes at once", async ({ page }) => {
  await mockApi(page);
  let current: Record<string, unknown> = { edition: "community", state: "community", source: "none", features: [], catalog: [] };
  let sent: unknown;
  await page.route((url) => url.pathname === "/api/license", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(current) }));
  await page.route((url) => url.pathname === "/api/admin/license", async (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    sent = route.request().postDataJSON();
    current = { ...enterpriseLicense, source: "console" };
    return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(current) });
  });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await expect(tab.getByRole("button", { name: "Install key" })).toBeDisabled();
  await tab.getByLabel("License key").fill("MVX1.payload.signature");
  await tab.getByRole("button", { name: "Install key" }).click();
  await expect(tab).toContainText("Installed: the deployment runs the Enterprise Edition.");
  expect(sent).toEqual({ key: "MVX1.payload.signature" });
  await expect(tab).toContainText("Installed on this page");
  await expect(tab.getByRole("button", { name: "Remove key" })).toBeVisible();
  await account(page).click();
  await expect(page.getByTestId("edition")).toHaveText(/Enterprise Edition/);
});

test("a key installed on the page is removed there, after a confirmation", async ({ page }) => {
  await mockApi(page, { license: { ...enterpriseLicense, source: "console" } });
  let removed = false;
  await page.route((url) => url.pathname === "/api/admin/license", (route) => {
    if (route.request().method() !== "DELETE") return route.fallback();
    removed = true;
    return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ edition: "community", state: "community", source: "none", features: [], catalog: [] }) });
  });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "License", exact: true }).click();
  await page.getByTestId("license-tab").getByRole("button", { name: "Remove key" }).click();
  await page.getByRole("button", { name: "Remove key" }).last().click();
  await expect.poll(() => removed).toBe(true);
});

test("a refused key says why", async ({ page }) => {
  await mockApi(page);
  await page.route((url) => url.pathname === "/api/admin/license", (route) => route.fulfill({ status: 400, contentType: "application/json",
    body: JSON.stringify({ error: "this license key expired on 2026-01-01; install the renewed key" }) }));
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await tab.getByLabel("License key").fill("MVX1.old.key");
  await tab.getByRole("button", { name: "Install key" }).click();
  await expect(tab).toContainText("expired on 2026-01-01");
});

test("a key set in the server's configuration is changed there, not on the page", async ({ page }) => {
  await mockApi(page, { license: enterpriseLicense });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await expect(tab.getByTestId("license-from-config")).toContainText("MAVERICKS_LICENSE_KEY");
  await expect(tab.getByLabel("License key")).toHaveCount(0);
  await expect(tab.getByRole("button", { name: "Remove key" })).toHaveCount(0);
});

// The License page gives the expiry date alone, no "(in N days)" (owner,
// 2026-11-22); the renewal notice 30 days ahead says what to do.
test("the expiry date stands alone, with no days-left count", async ({ page }) => {
  await mockApi(page, { license: { ...enterpriseLicense, renewal_due: true, expires_at: "2026-11-24T23:59:59Z" } });
  await loadAs(page, "platform_admin");
  await page.getByRole("button", { name: "License", exact: true }).click();
  const tab = page.getByTestId("license-tab");
  await expect(tab).toContainText("11/24/2026");
  await expect(tab).not.toContainText(/\(in \d+ days?\)|days ago|\(today\)/);
  await expect(tab).toContainText("from the day after, the deployment and every tenant run the Community Edition");
});
