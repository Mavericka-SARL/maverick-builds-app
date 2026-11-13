/**
 * A build's plan as the developer follows it (owner, 2026-11-12): one module
 * per stage, each done stage with the rows and periods compared, what every
 * stage took (minutes, calls, tokens, corrections), the decision waiting for
 * the developer, and how many verification checks are recorded.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SESSION_ID = "sess-plan";

test("the plan shows each stage's evidence and measure, the open decision and the checks", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const session = { id: SESSION_ID, app_id: "app-1", llm_provider: "openai", llm_model: "gpt-6.1-sol", created_at: "2026-11-12T09:00:00Z",
    title: "High-Tech build", draft_revision_id: "rev-draft", llm_calls: 50, plan_done: 1, plan_stages: 3 };
  const measure = (minutes: number, calls: number, corrections: number) =>
    ({ turns: 3, minutes, calls, input_tokens: 90_000, cached_tokens: 1_100_000, output_tokens: 10_000, tool_calls: 12, corrections });
  await page.route("**/api/ai/sessions", (route) =>
    route.request().method() === "GET" ? route.fulfill(json([session])) : route.fallback());
  await page.route("**/api/ai/settings", (route) => route.fulfill(json({ provider: "openai", model: "gpt-6.1-sol", has_key: true })));
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, (route) => route.fulfill(json({
    session, messages: [], documents: [],
    task_plan: {
      goal: "Build the FP&A model",
      stages: [
        { title: "Hiring Plan → Headcount → payroll", items: ["hiring loaded", "FTE", "payroll"], status: "done", sheets: ["Hiring Plan", "Headcount"],
          evidence: { built: "hiring and headcount grids", compared: "Headcount 18/18 rows, 648/648 periods matching", differences: "none", next: "SaaS" },
          measure: measure(12.4, 41, 0) },
        { title: "SaaS", items: ["subscriptions", "revenue"], status: "in_progress", sheet: "SaaS", measure: measure(3, 9, 1) },
        { title: "Import BS", items: ["closing balances"], status: "pending" },
      ],
      target: {
        summary: "Startup FP&A",
        decisions: [{ question: "Do imported closing balances start the next forecast?", options: ["no", "yes"], sheets: ["Import BS"] }],
        checks: [{ name: "Funding disabled", how: "switch off", status: "passed" }, { name: "Month 13 salary rise", how: "compare payroll" }],
      },
    },
  })));
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, (route) => route.fulfill(json([])));
  await page.route(`**/api/ai/sessions/${SESSION_ID}/workbook-map`, (route) => route.fulfill(json({ sheets: [], rows: [] })));

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /High-Tech build/ }).first().click();

  // The session list says what the session is: its draft, its plan's progress and calls.
  await expect(page.getByText("2026-11-12 · draft · 1/3 stages · 50 calls")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Start a new draft" })).toHaveCount(0);

  // Collapsed by default: the goal and progress, the decision waiting, the current stage.
  await expect(page.getByText("Plan: 1 of 3 stages done — Build the FP&A model")).toBeVisible();
  await expect(page.getByText("· 15 min, 50 calls, 1 correction so far")).toBeVisible();
  await expect(page.getByText("· verification checks: 1 of 2 recorded")).toBeVisible();
  await expect(page.getByText("Waiting for your decision: Do imported closing balances start the next forecast? (no / yes)")).toBeVisible();
  await expect(page.getByText("2. SaaS")).toBeVisible();
  await expect(page.getByText("1. Hiring Plan → Headcount → payroll")).toBeHidden();
  await expect(page.getByText("3. Import BS")).toBeHidden();

  // Expanded: one line per stage; a stage's evidence and measure open on a click.
  await page.getByRole("button", { name: "Expand the plan" }).click();
  await expect(page.getByText("3. Import BS")).toBeVisible();
  await expect(page.getByText("compared: Headcount 18/18 rows, 648/648 periods matching")).toBeHidden();
  await page.getByText("1. Hiring Plan → Headcount → payroll").click();
  await expect(page.getByText("compared: Headcount 18/18 rows, 648/648 periods matching")).toBeVisible();
  await expect(page.getByText("12 min · 41 calls · 1.2M tokens · 0 corrections")).toBeVisible();
  await page.getByText("2. SaaS").click();
  await expect(page.getByText("3 min · 9 calls · 1.2M tokens · 1 correction")).toBeVisible();
  await page.getByRole("button", { name: "Collapse the plan" }).click();
  await expect(page.getByText("3. Import BS")).toBeHidden();
});
