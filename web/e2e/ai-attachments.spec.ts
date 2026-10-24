/**
 * Attaching files to the AI Developer (a tester's report, 2026-10-24): the
 * picker takes several files at once, files can be dropped on the input
 * bar, a draft is continued or a new one started from the sidebar, and files
 * sent with a message leave the composer — they stay in the session, shown
 * on request, instead of looking attached to the next message.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SESSION_ID = "sess-files";
const DRAFT_ID = "draft-open";

const session = { id: SESSION_ID, app_id: "app-1", llm_provider: "openai", llm_model: "gpt-5-mini",
  draft_revision_id: DRAFT_ID, created_at: "2026-10-24T09:00:00Z" };

async function mockAi(page: Page) {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const uploaded: string[] = [];
  const created: string[] = [];
  await page.route("**/api/ai/sessions*", (route) => {
    const req = route.request();
    if (req.method() === "GET") return route.fulfill(json([session]));
    created.push(req.url());
    return route.fulfill(json({ ...session, id: `sess-${created.length}` }));
  });
  await page.route("**/api/ai/settings", (route) => route.fulfill(json({ provider: "openai", model: "gpt-5-mini", has_key: true })));
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, (route) =>
    route.request().method() === "GET" ? route.fulfill(json({ session, messages: [], documents: [] })) : route.fallback());
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, (route) => route.fulfill(json([])));
  await page.route(`**/api/ai/sessions/${SESSION_ID}/documents`, async (route) => {
    const name = /filename="([^"]+)"/.exec(route.request().postDataBuffer()?.toString("latin1") ?? "")?.[1] ?? "?";
    uploaded.push(name);
    await route.fulfill(json({ id: `doc-${uploaded.length}`, session_id: SESSION_ID, filename: name, mime_type: "text/csv",
      char_count: 10, truncated: false, importable: true, created_at: new Date().toISOString() }));
  });
  await page.route(`**/api/ai/sessions/${SESSION_ID}/messages*`, (route) => {
    const at = new Date().toISOString();
    const reply = { id: "m2", session_id: SESSION_ID, role: "assistant", content: "Read them.", created_at: at };
    const messages = [{ id: "m1", session_id: SESSION_ID, role: "user", content: "load these", created_at: at }, reply];
    return route.fulfill({ status: 200, contentType: "text/event-stream", body: `event: done\ndata: ${JSON.stringify({ reply, messages, session })}\n\n` });
  });
  return { uploaded, created };
}

test("several files at once, dropped files, and sent files leave the composer", async ({ page }) => {
  await mockApi(page);
  const { uploaded, created } = await mockAi(page);
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Session/ }).first().click();
  await expect(page.getByText("Working in your AI draft")).toBeVisible({ timeout: 10_000 });

  // The picker takes two files in one go.
  const csv = (name: string) => ({ name, mimeType: "text/csv", buffer: Buffer.from("code,label\nA,a\n") });
  await page.locator('input[type="file"]').setInputFiles([csv("members_sku.csv"), csv("members_node.csv")]);
  await expect(page.getByText("members_node.csv")).toBeVisible();
  expect(uploaded).toEqual(["members_sku.csv", "members_node.csv"]);

  // A file dropped on the input bar is attached as well; another type is refused by name.
  const drop = await page.evaluateHandle(() => {
    const dt = new DataTransfer();
    dt.items.add(new File(["code,label\n"], "data.csv", { type: "text/csv" }));
    dt.items.add(new File(["x"], "photo.png", { type: "image/png" }));
    return dt;
  });
  const bar = page.getByLabel("Message").locator("xpath=ancestor::div[contains(@style,'border-top')][1]");
  await bar.dispatchEvent("dragover", { dataTransfer: drop });
  await bar.dispatchEvent("drop", { dataTransfer: drop });
  await expect(page.getByText("data.csv")).toBeVisible();
  await expect(page.getByText("Not attached: photo.png")).toBeVisible();
  expect(uploaded).toEqual(["members_sku.csv", "members_node.csv", "data.csv"]);

  // Sent with a message, they leave the composer and stay in the session.
  await page.getByLabel("Message").fill("load these");
  await page.getByLabel("Message").press("Enter");
  await expect(page.getByText("Read them.")).toBeVisible();
  await expect(page.getByText("members_sku.csv")).toBeHidden();
  await page.getByRole("button", { name: "Show the session's files (3)" }).click();
  await expect(page.getByText("members_sku.csv")).toBeVisible();

  // With a draft open, a new session continues it unless a new draft is asked for.
  await page.getByRole("button", { name: "Start a new draft" }).click();
  await expect.poll(() => created.at(-1) ?? "").toContain("new_draft=1");
  await page.getByRole("button", { name: "New session", exact: true }).click();
  await expect.poll(() => created.length).toBe(2);
  expect(created[1]).not.toContain("new_draft");
});
