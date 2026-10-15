/**
 * A sign-in that has ended — its session expired, or was ended from
 * Keycloak — sends the console back to the sign-in page. The console used
 * to go on sending its expired token: every request failed, nothing said
 * why, and work in progress (a long import, reported live) failed with it.
 *
 * The page's clock is installed before the console loads, so it can be
 * moved past the access token's lifetime without waiting five minutes.
 */
import { test, expect } from "@playwright/test";

const KEYCLOAK_URL = process.env.VITE_KEYCLOAK_URL ?? "http://localhost:8180";
const REALM = process.env.VITE_KEYCLOAK_REALM ?? "mavericks";

async function adminToken(): Promise<string> {
  const res = await fetch(`${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ grant_type: "password", client_id: "admin-cli", username: "admin", password: "admin" }),
  });
  if (!res.ok) throw new Error(`keycloak admin login failed: ${res.status}`);
  return ((await res.json()) as { access_token: string }).access_token;
}

test("an ended sign-in sends the console back to the sign-in page", async ({ page }) => {
  await page.clock.install();
  const me = page.waitForResponse((res) => res.url().includes("/api/me") && res.request().method() === "GET");
  await page.goto("/");
  await page.waitForURL(/\/realms\/mavericks\/protocol\/openid-connect\/auth/);
  await page.fill("#username", "alex");
  await page.fill("#password", "mavericks");
  await page.click("#kc-login");
  await page.waitForURL((u) => !u.pathname.includes("/realms/"));
  expect((await me).status()).toBe(200);

  // End alex's sign-in from Keycloak, as an expired session or a sign-out
  // elsewhere does.
  const token = await adminToken();
  const users = (await (await fetch(`${KEYCLOAK_URL}/admin/realms/${REALM}/users?username=alex&exact=true`, {
    headers: { Authorization: `Bearer ${token}` },
  })).json()) as Array<{ id: string }>;
  const logout = await fetch(`${KEYCLOAK_URL}/admin/realms/${REALM}/users/${users[0].id}/logout`, {
    method: "POST", headers: { Authorization: `Bearer ${token}` },
  });
  expect(logout.status).toBe(204);

  // Past the access token's lifetime: the console's renewal is refused, and
  // it signs in again.
  await page.clock.fastForward("06:00");
  await page.waitForURL(/\/realms\/mavericks\/protocol\/openid-connect\/auth/, { timeout: 20_000 });
  await expect(page.locator("#username")).toBeVisible();
});
