import { test, expect } from "@playwright/test";

// Proves the full P0-1 chain works with a real token, not just each half in
// isolation: a real Keycloak PKCE login (mavericks-web client, seeded
// "alex"/"mavericks" user) followed by an authenticated API call that only
// succeeds if the gateway actually validated the Bearer token against JWKS
// (internal/gateway/handler.go's resolveJWTActor) and client.ts actually
// sent it (web/src/api/client.ts's authHeader()). Negative auth scenarios
// (expired/wrong-issuer tokens) are deliberately not re-driven through the
// browser here — that's the same JWKSValidator.Validate logic already
// covered directly and deterministically by internal/gateway's Go tests.
test("real Keycloak login authenticates and an API call succeeds", async ({ page }) => {
  const apiResponse = page.waitForResponse(
    (res) => res.url().includes("/api/") && res.request().method() === "GET",
  );

  await page.goto("/");

  // keycloak-js's onLoad: "login-required" redirects immediately to
  // Keycloak's own hosted login form.
  await page.waitForURL(/\/realms\/mavericks\/protocol\/openid-connect\/auth/);
  await page.fill("#username", "alex");
  await page.fill("#password", "mavericks");
  await page.click("#kc-login");

  // Back on the app, past ProdAuthProvider's `if (!authenticated) return null` gate.
  await page.waitForURL("http://localhost:5173/**");

  const res = await apiResponse;
  expect(res.status()).toBe(200);
  const authHeader = res.request().headers()["authorization"];
  expect(authHeader).toMatch(/^Bearer .+/);
});
