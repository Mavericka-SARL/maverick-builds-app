/**
 * What an AI Developer session carries for the developer to see (High-Tech
 * build, 2026-11-11): the workbook map — which row became which metric, what
 * differs, what is not mapped yet — the decisions kept from earlier answers,
 * what the calls cost when the deployment prices the model, and a call budget
 * the developer sets on the session.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SESSION_ID = "sess-map";

test("the session shows its workbook map, kept decisions, cost and call budget", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const session = { id: SESSION_ID, app_id: "app-1", llm_provider: "openai", llm_model: "gpt-5.4-mini", created_at: "2026-11-11T09:00:00Z" };
  let budget: unknown = "unset";
  await page.route("**/api/ai/sessions", (route) =>
    route.request().method() === "GET" ? route.fulfill(json([session])) : route.fallback());
  await page.route("**/api/ai/settings", (route) => route.fulfill(json({ provider: "openai", model: "gpt-5.4-mini", has_key: true })));
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, async (route) => {
    if (route.request().method() === "PATCH") {
      budget = (route.request().postDataJSON() as { call_budget: unknown }).call_budget;
      return route.fulfill(json({ id: SESSION_ID, call_budget: budget }));
    }
    await route.fulfill(json({
      session, messages: [{ id: "m1", session_id: SESSION_ID, role: "user", content: "compare the Headcount rows", created_at: "2026-11-11T09:01:00Z" }],
      documents: [],
      usage: { llm_calls: 6, input_tokens: 69287, output_tokens: 278, cache_read_tokens: 113664, cache_write_tokens: 0, estimated_cost_usd: 0.04 },
      task_plan: { goal: "", stages: [], decisions: ["Asked: Percent units or fractions? — the developer answered: percent units"] },
    }));
  });
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, (route) => route.fulfill(json([])));
  await page.route(`**/api/ai/sessions/${SESSION_ID}/workbook-map`, (route) => route.fulfill(json({
    sheets: [{ file: "model.xlsx", sheet: "Headcount", mapped: 3, match: 1, differ: [8, 9], not_mapped: [10, 14] }],
    rows: [],
  })));

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Session/ }).first().click();

  await expect(page.getByText("6 calls, 183.0k tokens in, 278 out")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText("about USD 0.04")).toBeVisible();

  await page.getByRole("button", { name: /Workbook map: 3 rows mapped, 1 match, 2 differ, 2 with numbers not mapped/ }).click();
  await expect(page.getByText("— differ: rows 8, 9")).toBeVisible();
  await expect(page.getByText("— not mapped: rows 10, 14")).toBeVisible();

  await page.getByRole("button", { name: "Decisions kept: 1" }).click();
  await expect(page.getByText("the developer answered: percent units")).toBeVisible();

  await page.getByRole("button", { name: "· set a call budget" }).click();
  await page.getByLabel("Call budget").fill("40");
  await page.getByRole("button", { name: "Set", exact: true }).click();
  await expect.poll(() => budget).toBe(40);
  await expect(page.getByText("6 of 40 calls")).toBeVisible();
});
