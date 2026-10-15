/**
 * A personal API token works where only Keycloak sign-ins did
 * (DEV_MODE=false): a person signs in, makes a token under Account › API
 * tokens, and a script — no browser, no cookie — reads as them with it,
 * until it is revoked.
 */
import { test, expect, request } from "@playwright/test";

const gatewayPort = process.env.E2E_GATEWAY_PORT ?? "8080";

test("a token made in the console lets a script read as its owner, until revoked", async ({ page }) => {
  await page.goto("/");
  await page.waitForURL(/\/realms\/mavericks\/protocol\/openid-connect\/auth/);
  await page.fill("#username", "alex");
  await page.fill("#password", "mavericks");
  await page.click("#kc-login");
  await page.waitForURL((u) => !u.pathname.includes("/realms/"));

  await page.getByRole("button", { name: /^Account:/ }).click();
  await page.getByRole("menuitem", { name: "API tokens" }).click();
  const dialog = page.getByRole("dialog", { name: "API tokens" });
  await dialog.getByLabel("Name").fill("e2e script");
  await dialog.getByRole("button", { name: "Create token" }).click();
  const token = await dialog.getByRole("textbox", { name: "Token", exact: true }).inputValue();
  expect(token).toMatch(/^mvx_pat_/);

  // The script: straight to the gateway, the token alone.
  const script = await request.newContext({ baseURL: `http://localhost:${gatewayPort}` });
  const me = await script.get("/api/me", { headers: { Authorization: `Bearer ${token}` } });
  expect(me.status()).toBe(200);
  expect((await me.json()).email).toBeTruthy();
  const write = await script.patch("/api/me/preferences", { headers: { Authorization: `Bearer ${token}` }, data: { theme: "dark" } });
  expect(write.status(), "a read-only token changed something").toBe(403);

  const row = dialog.getByRole("row", { name: /e2e script/ });
  await row.getByRole("button", { name: "Revoke e2e script" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Revoke" }).click();
  await expect(dialog.getByText("You have no API tokens.")).toBeVisible();
  const after = await script.get("/api/me", { headers: { Authorization: `Bearer ${token}` } });
  expect(after.status()).toBe(401);
  await script.dispose();
});
