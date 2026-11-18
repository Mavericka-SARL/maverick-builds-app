/**
 * One Workflow page (owner, 2026-11-18): the Inbox and the History are two
 * views of it, and the History is searched by submitter, status, stage and
 * model, as the Inbox is by anything its card shows. Older runs are a page
 * away ("Load older runs", GET ...?before=<last run's id>). Mocked: CI runs
 * without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test.use({ actionTimeout: 10_000 });

const step = (id: string, step_name: string, status: string, decision?: string) =>
  ({ id, step_def_id: id, step_name, step_type: "approval", status, decision });
const run = (id: string, workflow_name: string, status: string, submitted_by: string, model_name: string, steps: unknown[]) =>
  ({ id, workflow_name, status, submitted_by, model_name, context: {}, started_at: "2026-11-10T09:00:00Z", steps });

// A full first page, so the History offers the next one.
const firstPage = [
  run("r-1", "Budget round", "running", "Rosa Lind", "Restaurant FP&A", [step("s1", "Submit", "completed", "approve"), step("s2", "CFO review", "in_progress")]),
  run("r-2", "Monthly close", "completed", "Theo Marchetti", "Creator & Agency FP&A", [step("s3", "Approve the pack", "completed", "approve")]),
  ...Array.from({ length: 48 }, (_, i) =>
    run(`f-${i}`, "Forecast refresh", "completed", "Theo Marchetti", "Restaurant FP&A", [step(`fs-${i}`, "Load actuals", "completed")])),
];
const olderPage = [
  run("r-old", "Capex request", "failed", "Ana Pushkova", "Restaurant FP&A", [step("s9", "Board approval", "skipped")]),
];

test("history: one Workflow page, searched by submitter, stage, model and status, older runs a page away", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const asked: string[] = [];
  await page.route("**/api/workflow/my-history**", (route) => {
    const before = new URL(route.request().url()).searchParams.get("before") ?? "";
    asked.push(before);
    return route.fulfill(json(before === "f-47" ? olderPage : before ? [] : firstPage));
  });

  await loadAs(page, "dept_head");
  const nav = page.getByRole("navigation", { name: "Primary" });
  await expect(nav.getByRole("button", { name: "Workflow Inbox" })).toHaveCount(0);
  await expect(nav.getByRole("button", { name: "Workflow History" })).toHaveCount(0);
  await nav.getByRole("button", { name: "Workflow", exact: true }).click();
  await page.getByRole("button", { name: "History", exact: true }).click();
  await expect(page.getByText("Budget round")).toBeVisible({ timeout: 15_000 });
  // Each run says its model and who submitted it.
  await expect(page.getByText("Restaurant FP&A · submitted by Rosa Lind")).toBeVisible();

  const search = page.getByLabel("Search workflow history");
  await search.fill("rosa cfo review");
  await expect(page.getByText("Budget round")).toBeVisible();
  await expect(page.getByText("Monthly close")).toHaveCount(0);

  await search.fill("");
  await page.getByLabel("Filter by stage").selectOption("CFO review");
  await expect(page.getByText("Budget round")).toBeVisible();
  await expect(page.getByText("Forecast refresh")).toHaveCount(0);
  await page.getByRole("button", { name: "Clear", exact: true }).click();

  await page.getByLabel("Filter by model").selectOption("Creator & Agency FP&A");
  await expect(page.getByText("Monthly close")).toBeVisible();
  await expect(page.getByText("Budget round")).toHaveCount(0);
  await page.getByLabel("Filter by submitter").selectOption("Theo Marchetti");
  await page.getByLabel("Filter by status").selectOption("completed");
  await expect(page.getByText("Monthly close")).toBeVisible();
  await page.getByRole("button", { name: "Clear", exact: true }).click();

  // A run older than the first page: not found until the next page is read.
  await search.fill("pushkova");
  await expect(page.getByText(/No run matches among the latest 50/)).toBeVisible();
  await page.getByRole("button", { name: "Load older runs" }).click();
  await expect(page.getByText("Capex request")).toBeVisible();
  expect(asked).toContain("f-47");
  await expect(page.getByRole("button", { name: "Load older runs" })).toHaveCount(0);

  // The view is part of the remembered tab: a reload reopens the History.
  await page.reload();
  await expect(page.getByRole("button", { name: "History", exact: true })).toHaveAttribute("aria-pressed", "true", { timeout: 15_000 });
});
