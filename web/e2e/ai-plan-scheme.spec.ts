/**
 * The plan's architecture scheme (owner, 2026-11-16): a click holds a box's
 * links lit, so the developer can follow its lines to the joined boxes
 * without the highlight going when the pointer leaves the box; Full screen
 * opens the scheme beside the left panel, the held box still held.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const SESSION_ID = "sess-scheme";

test("a clicked box keeps its links lit, and the scheme opens full screen", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const session = { id: SESSION_ID, app_id: "app-1", llm_provider: "openai", llm_model: "gpt-5-mini", created_at: "2026-11-16T09:00:00Z",
    title: "Territory build", draft_revision_id: "rev-draft", plan_done: 0, plan_stages: 1 };
  await page.route("**/api/ai/sessions", (route) =>
    route.request().method() === "GET" ? route.fulfill(json([session])) : route.fallback());
  await page.route("**/api/ai/settings", (route) => route.fulfill(json({ provider: "openai", model: "gpt-5-mini", has_key: true })));
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, (route) => route.fulfill(json({
    session, messages: [], documents: [],
    task_plan: {
      goal: "Build the territory model",
      stages: [{ title: "Accounts and quotas", items: ["accounts", "quota"], status: "in_progress" }],
      target: {
        dimensions: [{ name: "account", members: "86 source ids" }, { name: "scenario" }, { name: "seller" }],
        sheets: [
          { sheet: "Assumptions", role: "input", grid: "Assumptions", dimensions: [], metrics: ["planning_year"] },
          { sheet: "Accounts", role: "calculation", grid: "Account opportunity", dimensions: ["account", "scenario"],
            metrics: ["name", "region", "segment", "industry", "territory", "proposed", "arr", "whitespace"] },
          { sheet: "Quota", role: "input", grid: "Quota", dimensions: ["seller"], metrics: ["quota"] },
        ],
        pages: [
          { name: "Start here", guide: true },
          { name: "Executive Dashboard", audience: "CFO", grids: ["Account opportunity"] },
          { name: "Territory Plan", grids: ["Quota"] },
        ],
      },
    },
  })));
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, (route) => route.fulfill(json([])));
  await page.route(`**/api/ai/sessions/${SESSION_ID}/workbook-map`, (route) => route.fulfill(json({ sheets: [], rows: [] })));

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Territory build/ }).first().click();
  await page.getByRole("button", { name: "Expand the plan" }).click();

  const box = (scope: ReturnType<typeof page.locator>, id: string) => scope.locator(`[data-scheme-box="${id}"]`);
  const inline = page.locator("body");
  const account = box(inline, "dim:account").first();
  const opportunity = box(inline, "grid:account opportunity").first();
  const quota = box(inline, "grid:quota").first();
  const executive = box(inline, "page:executive dashboard").first();
  await expect(account).toBeVisible({ timeout: 10_000 });

  // Held: the pointer goes to another box and the held box's links stay lit.
  await account.click();
  await expect(account).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByText("Its links stay lit: click the box again, or an empty space, to let go.")).toBeVisible();
  await quota.hover();
  await expect(opportunity).toHaveCSS("opacity", "1");
  await expect(executive).toHaveCSS("opacity", "1");
  await expect(quota).toHaveCSS("opacity", "0.35");

  // "+2 more" opens the metrics and leaves the held box held.
  await opportunity.getByRole("button", { name: "+2 more" }).click();
  await expect(opportunity.getByText(/whitespace/)).toBeVisible();
  await expect(account).toHaveAttribute("aria-pressed", "true");

  // Clicking it again lets go; so does an empty space.
  await account.click();
  await expect(account).toHaveAttribute("aria-pressed", "false");
  await box(inline, "dim:seller").first().click();
  await page.getByText("Dimensions (3)").first().click();
  await expect(box(inline, "dim:seller").first()).toHaveAttribute("aria-pressed", "false");

  // The keyboard holds a box too, and Full screen keeps it held.
  await box(inline, "grid:quota").first().focus();
  await page.keyboard.press("Enter");
  await expect(quota).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("button", { name: "Full screen" }).click();
  const full = page.getByRole("dialog", { name: "Architecture" });
  await expect(full).toBeVisible();
  await expect(box(full, "grid:quota")).toHaveAttribute("aria-pressed", "true");
  await expect(box(full, "page:territory plan")).toHaveCSS("opacity", "1");
  await expect(box(full, "grid:account opportunity")).toHaveCSS("opacity", "0.35");
  const wide = await full.boundingBox();
  expect(wide!.height).toBeGreaterThan(500);

  // Letting go in full screen lets go in the plan as well; Escape closes it.
  await box(full, "grid:quota").click();
  await page.keyboard.press("Escape");
  await expect(full).toBeHidden();
  await expect(quota).toHaveAttribute("aria-pressed", "false");
  await expect(page.getByRole("button", { name: "Full screen" })).toBeFocused();
});
