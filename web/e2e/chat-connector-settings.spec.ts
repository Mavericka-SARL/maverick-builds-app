/**
 * Tenant admin › Chat connector (web/src/consoles/admin/ChatConnectorSettingsTab.tsx):
 * a tenant administrator decides whether the tenant's people may change grid
 * values from ChatGPT or Claude. The switch says what it covers and saves at
 * once; a business user has no such tab.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("a tenant administrator turns chat writes off", async ({ page }) => {
  await mockApi(page);
  let state = { chat_writes: true, scope: { customer_id: "c-1", deployment: false, inherited: false, deployment_settings_available: false } };
  const puts: unknown[] = [];
  await page.route("**/api/admin/connector-settings", async (route) => {
    if (route.request().method() === "PUT") {
      const body = route.request().postDataJSON() as { chat_writes: boolean };
      puts.push(body);
      state = { ...state, chat_writes: body.chat_writes };
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(state) });
  });
  await loadAs(page, "tenant_admin");
  await page.getByRole("button", { name: "Chat connector", exact: true }).click();

  const panel = page.getByTestId("connector-settings");
  await expect(panel).toContainText("under their own access rules and workflow locks");
  const box = panel.getByRole("checkbox", { name: "Allow changing grid values from a chat" });
  await expect(box).toBeChecked();
  await box.uncheck();
  await expect(box).not.toBeChecked();
  expect(puts).toEqual([{ chat_writes: false }]);
  await page.screenshot({ path: test.info().outputPath("chat-connector-settings.png") });
});

test("a business user has no chat connector settings", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "dept_head");
  await expect(page.getByRole("button", { name: "Chat connector", exact: true })).toHaveCount(0);
});
