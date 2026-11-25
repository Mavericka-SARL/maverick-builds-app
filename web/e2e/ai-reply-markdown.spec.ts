/**
 * The AI Developer's replies are shown with the small Markdown its prompt
 * names (ui/RichText): bold, numbered lists, code. A reply's "**one field**"
 * used to show its stars (2026-11-25). A formula's stars stay as written —
 * "a * b * c" and "Price*Qty*12" are not italic — and a numbered list whose
 * items are a blank line apart reads 1, 2, 3. The developer's own message
 * shows as typed.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SESSION_ID = "sess-md";
const session = { id: SESSION_ID, app_id: "app-1", llm_provider: "openai", llm_model: "gpt-5-mini", created_at: "2026-11-25T09:00:00Z" };
const REPLY = [
  "Understood: **one “Contacts” field in Deals**, allowing several contacts.",
  "",
  "1. Make a table **Deal contacts**.",
  "",
  "2. Give it a Deal field and a Contact field.",
  "",
  "3. Each deal's page lists its contacts.",
  "",
  "The total is Revenue * Price * 12, or Price*Qty*12, or `Units * Rate`.",
].join("\n");

test("an AI reply shows its Markdown; formulas and the developer's words show as written", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/ai/sessions", (route) =>
    route.request().method() === "GET" ? route.fulfill({ json: [session] }) : route.fallback());
  await page.route("**/api/ai/settings", (route) => route.fulfill({ json: { provider: "openai", model: "gpt-5-mini", has_key: true } }));
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, (route) =>
    route.request().method() === "GET"
      ? route.fulfill({ json: { session, documents: [], messages: [
        { id: "u1", session_id: SESSION_ID, role: "user", content: "create **Contacts** in Deals", created_at: "2026-11-25T09:01:00Z" },
        { id: "a1", session_id: SESSION_ID, role: "assistant", content: REPLY, created_at: "2026-11-25T09:01:05Z" },
      ] } })
      : route.fallback());
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, (route) => route.fulfill({ json: [] }));

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Session/ }).first().click();

  const reply = page.locator(".mvx-chat-bubble:not(.mvx-chat-bubble--user)").filter({ hasText: "Understood" });
  await expect(reply).toBeVisible({ timeout: 10_000 });
  await expect(reply.locator("strong").first()).toHaveText("one “Contacts” field in Deals");
  await expect(reply).not.toContainText("**");
  // Items a blank line apart are one list in order, not three lists of "1.".
  const items = reply.locator("ol > li");
  await expect(items).toHaveCount(3);
  const starts = await reply.locator("ol").evaluateAll(ols => ols.map(ol => [(ol as HTMLOListElement).start, ol.children.length]));
  expect(starts).toEqual([[1, 1], [2, 1], [3, 1]]);
  // A formula's stars are not emphasis.
  await expect(reply).toContainText("Revenue * Price * 12, or Price*Qty*12");
  await expect(reply.locator("em")).toHaveCount(0);
  await expect(reply.locator("code")).toHaveText("Units * Rate");

  const mine = page.locator(".mvx-chat-bubble--user").filter({ hasText: "Contacts" });
  await expect(mine).toHaveText("create **Contacts** in Deals");
});
