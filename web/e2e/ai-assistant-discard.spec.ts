/**
 * Smoke test: the AI Developer's discard-draft flow and Activity panel
 * (web/src/consoles/developer/AIAssistant.tsx). Discard used to call the
 * generic DELETE /api/developer/revisions/{id} and only clear
 * draft_revision_id in local React state — never server-side — so this test
 * asserts the new POST .../discard-draft endpoint is what the button
 * actually calls, and that the response (not local guesswork) drives the
 * "AI draft" banner disappearing. The Activity panel (GET .../proposals) is
 * this item's other new frontend surface — the only history of AI actions
 * beyond the linear chat transcript.
 *
 * Uses the same purpose-built mock as the other specs in this directory (no
 * live Go gateway in the e2e CI job — see business-console-grid-smoke's
 * header comment for why).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SESSION_ID = "sess-1";
const DRAFT_REV_ID = "draft-rev-1";

function session(withDraft: boolean) {
  return {
    id: SESSION_ID,
    app_id: "app-1",
    llm_provider: "openai",
    llm_model: "gpt-4o-mini",
    draft_revision_id: withDraft ? DRAFT_REV_ID : undefined,
    created_at: "2026-08-01T09:00:00Z",
  };
}

const proposals = [
  {
    id: "prop-1",
    session_id: SESSION_ID,
    status: "executed",
    summary: "Executed 1 step successfully.",
    steps: [{ tool: "create_dimension", description: "Create dimension 'Region'", params: {}, status: "success", result: "Dimension 'Region' created (id: d-99)" }],
    created_at: "2026-08-01T09:05:00Z",
    executed_at: "2026-08-01T09:05:01Z",
  },
];

// Common AI routes every test in this file needs: session list, settings,
// session detail (always reporting a draft), and the proposals list.
async function mockCommonAiRoutes(page: Page) {
  await page.route("**/api/ai/sessions", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([session(true)]) });
  });
  await page.route("**/api/ai/settings", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ provider: "openai", model: "gpt-4o-mini", has_key: true }) });
  });
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ session: session(true), messages: [], documents: [] }) });
  });
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(proposals) });
  });
}

async function openAiAssistantSession(page: Page) {
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Session/ }).first().click();
}

test("Discard draft calls the real discard-draft endpoint and the banner disappears", async ({ page }) => {
  await mockApi(page);
  await mockCommonAiRoutes(page);
  const discardCalls: string[] = [];
  await page.route(`**/api/ai/sessions/${SESSION_ID}/discard-draft`, async (route) => {
    discardCalls.push(route.request().method());
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        session: session(false),
        messages: [{ id: "m-1", session_id: SESSION_ID, role: "assistant", content: "Discarded the draft — every change made in this session was removed.", created_at: "2026-08-01T09:10:00Z" }],
      }),
    });
  });

  await openAiAssistantSession(page);

  // Draft banner visible before discard.
  await expect(page.getByText("AI draft")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Discard" })).toBeVisible();

  await page.getByRole("button", { name: "Discard" }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Discard draft" }).click();

  await expect.poll(() => discardCalls.length).toBeGreaterThan(0);
  expect(discardCalls[0]).toBe("POST");

  // Banner gone — driven by the server response, not local guesswork.
  await expect(page.getByText("AI draft")).not.toBeVisible();
  await expect(page.getByText("Discarded the draft")).toBeVisible();
});

test("Activity panel lists past proposals with status and summary", async ({ page }) => {
  await mockApi(page);
  await mockCommonAiRoutes(page);

  await openAiAssistantSession(page);

  await page.getByRole("button", { name: "Activity" }).click();
  await expect(page.getByText("Executed 1 step successfully.")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText("executed", { exact: true })).toBeVisible();
});
