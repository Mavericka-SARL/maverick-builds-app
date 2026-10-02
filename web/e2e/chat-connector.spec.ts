/**
 * Account menu → "Connect ChatGPT or Claude" (web/src/shell/UserMenu.tsx,
 * ChatConnectorDialog.tsx): every signed-in person, a business user included,
 * finds the connector's URL and each host's client id and secret, the secret
 * masked until asked for. A deployment without the connector shows no item.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

async function openMenu(page: Page, connector: unknown) {
  await mockApi(page);
  await page.route("**/api/connector", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(connector) }));
  await loadAs(page, "dept_head");
  await page.getByRole("button", { name: /^Account/ }).click();
}

test("a business user finds how to connect ChatGPT and Claude in the account menu", async ({ page }) => {
  await openMenu(page, {
    enabled: true, url: "https://app.example.test/mcp", scope: "models:read",
    hosts: [
      { name: "ChatGPT", client_id: "chatgpt-connector", client_secret: "gpt-secret-123" },
      { name: "Claude", client_id: "claude-connector", client_secret: "claude-secret-456" },
    ],
  });
  await page.getByRole("menuitem", { name: "Connect ChatGPT or Claude" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Use your models in ChatGPT and Claude")).toBeVisible();
  await expect(dialog.getByLabel("Connector URL")).toHaveValue("https://app.example.test/mcp");
  await expect(dialog.getByLabel("Claude client ID")).toHaveValue("claude-connector");
  const secret = dialog.getByLabel("Claude client secret");
  await expect(secret).not.toHaveValue("claude-secret-456");
  await dialog.getByRole("button", { name: "Show" }).nth(1).click();
  await expect(secret).toHaveValue("claude-secret-456");
  await page.screenshot({ path: test.info().outputPath("connector-dialog.png") });
});

test("a deployment without the connector shows no item", async ({ page }) => {
  await openMenu(page, { enabled: false, hosts: [] });
  await expect(page.getByRole("menuitem", { name: "Sign out" })).toBeVisible();
  await expect(page.getByRole("menuitem", { name: "Connect ChatGPT or Claude" })).toHaveCount(0);
});
