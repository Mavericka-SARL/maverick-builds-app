/**
 * The AI Developer's turn controls (found rebuilding a sales target-setting
 * workbook: two turns ran past 15 minutes with no feedback and no way to stop
 * them, and a promoted session went on checking plans against its old base):
 * a running turn shows its time and a Stop button that closes the request; a
 * proposal carries the plan check's warnings; and a session whose draft was
 * promoted is finished — the input is closed and a new session offered.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SESSION_ID = "sess-turn";

function session(promoted: boolean) {
  return {
    id: SESSION_ID, app_id: "app-1", llm_provider: "openai", llm_model: "gpt-5-mini",
    created_at: "2026-10-05T09:00:00Z", ...(promoted ? { promoted_at: "2026-10-05T10:00:00Z" } : {}),
  };
}

async function mockAi(page: Page, opts: { promoted: boolean; messages?: unknown[]; proposals?: unknown[] }) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/ai/sessions", (route) =>
    route.request().method() === "GET" ? route.fulfill(json([session(opts.promoted)])) : route.fallback());
  await page.route("**/api/ai/settings", (route) => route.fulfill(json({ provider: "openai", model: "gpt-5-mini", has_key: true })));
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, (route) =>
    route.request().method() === "GET"
      ? route.fulfill(json({ session: session(opts.promoted), messages: opts.messages ?? [], documents: [] }))
      : route.fallback());
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, (route) => route.fulfill(json(opts.proposals ?? [])));
}

async function openSession(page: Page) {
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Session/ }).first().click();
}

test("a promoted session is finished: input closed, a new session offered", async ({ page }) => {
  await mockApi(page);
  await mockAi(page, { promoted: true });
  await openSession(page);
  await expect(page.getByText("This session's draft was promoted")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByLabel("Message")).toBeDisabled();
  await expect(page.getByRole("button", { name: "New session" }).last()).toBeVisible();
});

test("a running turn shows its time and stops on Stop", async ({ page }) => {
  await mockApi(page);
  let stopped = false;
  await mockAi(page, {
    promoted: false,
    messages: [],
  });
  await page.route(`**/api/ai/sessions/${SESSION_ID}/messages*`, async (route) => {
    // A turn that would run for minutes; the browser closes it on Stop.
    await new Promise(r => setTimeout(r, 8000));
    await route.fulfill({ status: 200, contentType: "text/event-stream", body: "" }).catch(() => undefined);
  });
  await openSession(page);
  await page.getByLabel("Message").fill("build every stage");
  await page.getByRole("button", { name: "Send" }).click();
  const stop = page.getByRole("button", { name: "Stop the turn" });
  await expect(stop).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText(/0:0[1-9]/)).toBeVisible({ timeout: 5_000 }); // the turn's running time
  // After Stop the transcript is read back: the server saved "Stopped".
  await page.unroute(`**/api/ai/sessions/${SESSION_ID}`);
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, (route) => {
    stopped = true;
    return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      session: session(false), documents: [],
      messages: [
        { id: "u1", session_id: SESSION_ID, role: "user", content: "build every stage", created_at: "2026-10-05T09:01:00Z" },
        { id: "a1", session_id: SESSION_ID, role: "assistant", content: "⏹ Stopped by the developer.", created_at: "2026-10-05T09:01:05Z" },
      ] }) });
  });
  await stop.click();
  await expect(page.getByText("Stopped by the developer.")).toBeVisible({ timeout: 10_000 });
  expect(stopped).toBe(true);
  await expect(page.getByRole("button", { name: "Send" })).toBeVisible();
});

test("a proposal shows the plan check's warnings", async ({ page }) => {
  await mockApi(page);
  await mockAi(page, { promoted: false });
  const proposal = {
    id: "prop-w", session_id: SESSION_ID, status: "pending", created_at: "2026-10-05T09:02:00Z",
    steps: [{ tool: "create_metric", description: "Create target", params: { name: "target", formula: "base * growth_pct" } }],
  };
  const warning = "step 1 (create_metric target): the formula multiplies growth_pct, a Percentage metric stored in percent units (6 for 6%), without dividing by 100";
  await page.route(`**/api/ai/sessions/${SESSION_ID}/messages*`, (route) => route.fulfill({
    status: 200, contentType: "text/event-stream",
    body: `event: progress\ndata: ${JSON.stringify({ tool: "propose_actions", chars: 2048, steps: 1 })}\n\n` +
      `event: proposal\ndata: ${JSON.stringify({ proposal, messages: [], session: session(false), warnings: [warning] })}\n\n`,
  }));
  await openSession(page);
  await page.getByLabel("Message").fill("create the target");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByText("The plan check warns:")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText(/without dividing by 100/)).toBeVisible();
});

test("a reopened session shows a proposal's warnings in Activity", async ({ page }) => {
  await mockApi(page);
  const warning = "step 1 (create_metric target): the formula multiplies growth_pct, a Percentage metric stored in percent units (6 for 6%), without dividing by 100";
  await mockAi(page, { promoted: false, proposals: [{
    id: "prop-w", session_id: SESSION_ID, status: "pending", created_at: "2026-10-05T09:02:00Z", summary: "Create target",
    steps: [{ tool: "create_metric", description: "Create target", params: { name: "target" } }], warnings: [warning],
  }] });
  await openSession(page);
  await page.getByRole("button", { name: "Activity" }).click();
  await expect(page.getByText("The plan check warned:")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText(/without dividing by 100/)).toBeVisible();
});
