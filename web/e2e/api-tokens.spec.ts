/**
 * Personal API tokens (Account › API tokens) and a revision's Recalculate.
 *
 * A script used to borrow the console's own sign-in token, which expires
 * within the hour: a person now makes a token of their own, sees it once,
 * and revokes it. And a revision whose recalculation was cut short (the
 * gateway killed mid-way) stayed stale until an input changed: its developer
 * recalculates it.
 *
 * The server side is internal/gateway/api_tokens_test.go and
 * revision_recalc_test.go.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

test("a person makes an API token, sees it once, and revokes it", async ({ page }) => {
  await mockApi(page);
  type Token = { id: string; name: string; hint: string; access: string; created_at: string; expires_at: string; last_used_at: string | null; expired: boolean };
  let tokens: Token[] = [];
  const made: Array<Record<string, unknown>> = [];
  let revoked = "";
  await page.route("**/api/me/api-tokens", async (route) => {
    if (route.request().method() === "POST") {
      const body = route.request().postDataJSON();
      made.push(body);
      const t = { id: "tok-1", name: body.name, hint: "mvx_pat_AbCd", access: body.access, created_at: "2026-10-15T09:00:00Z",
        expires_at: "2027-01-13T09:00:00Z", last_used_at: null, expired: false };
      tokens = [t];
      return route.fulfill(json({ token: "mvx_pat_AbCdEfGhIjKlMnOp", api_token: t }));
    }
    return route.fulfill(json(tokens));
  });
  await page.route("**/api/me/api-tokens/*", async (route) => {
    revoked = route.request().url().split("/").pop() ?? "";
    tokens = [];
    return route.fulfill(json({ revoked: true }));
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: /^Account:/ }).click();
  await page.getByRole("menuitem", { name: "API tokens" }).click();
  const dialog = page.getByRole("dialog", { name: "API tokens" });
  await expect(dialog.getByText("You have no API tokens.")).toBeVisible();

  await dialog.getByLabel("Name").fill("nightly load");
  await dialog.getByLabel("Access").selectOption("write");
  await dialog.getByLabel("Expires in").selectOption("90");
  await dialog.getByRole("button", { name: "Create token" }).click();

  await expect(dialog.getByText("it is shown only once")).toBeVisible();
  await expect(dialog.getByRole("textbox", { name: "Token", exact: true })).toHaveValue("mvx_pat_AbCdEfGhIjKlMnOp");
  expect(made[0]).toEqual({ name: "nightly load", access: "write", expires_in_days: 90 });
  const row = dialog.getByRole("row", { name: /nightly load/ });
  await expect(row).toContainText("Read and write");
  await expect(row).toContainText("never");

  await row.getByRole("button", { name: "Revoke nightly load" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Revoke" }).click();
  await expect.poll(() => revoked).toBe("tok-1");
  await expect(dialog.getByText("You have no API tokens.")).toBeVisible();
});

test("a developer recalculates a revision", async ({ page }) => {
  await mockApi(page);
  const LIVE = { id: "rev-live", name: "Plan live", created_at: "2026-10-01T00:00:00Z" };
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json([{
    id: "t-1", name: "Acme", plan: "test", created_at: "2026-01-01T00:00:00Z",
    applications: [{
      id: "app-1", name: "Planning", mode: "planning", status: "active",
      models: [{ id: "model-1", name: "In Order Planning", storage_type: "oltp", active_revision: LIVE.name, is_default: true, revisions: [LIVE] }],
    }],
  }])));
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...LIVE, is_active: true }])));
  let asked = "";
  await page.route("**/api/developer/revisions/*/recalculate", (route) => {
    asked = route.request().method() + " " + new URL(route.request().url()).pathname;
    return route.fulfill(json({ recalculating: true, metrics: 618 }));
  });

  await loadAs(page, "developer");
  const model = page.locator(".mvx-admin-model", { hasText: "In Order Planning" });
  await expect(model).toBeVisible({ timeout: 15_000 });
  await model.getByRole("button", { name: `Recalculate revision ${LIVE.name}` }).click();

  await expect.poll(() => asked).toBe("POST /api/developer/revisions/rev-live/recalculate");
  await expect(page.getByRole("status").filter({ hasText: "Recalculating 618 calculated metrics" })).toBeVisible();
});
