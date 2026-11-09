/**
 * The Workflow Inbox names each task's model and searches by anything its
 * card shows — workflow, model, step, its state, who asked — every word typed
 * matching (owner, 2026-11-09: the Inbox spans every application, and a card
 * did not say which model it was about). Mocked: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test.use({ actionTimeout: 10_000 });

const base = { context: {}, status: "in_progress", created_at: "2026-11-09T08:00:00Z", required_comment: false, completion_label: "Complete", instructions: "", step_type: "task" };
const tasks = [
  { ...base, id: "t-1", instance_id: "i-1", step_def_id: "c-gl", step_name: "Load GL postings", workflow_name: "Monthly close",
    model_name: "Creator & Agency FP&A", requested_by: "Theo Marchetti" },
  { ...base, id: "t-2", instance_id: "i-2", step_def_id: "c-approve", step_name: "Approve the monthly pack", step_type: "approval",
    workflow_name: "Monthly close", model_name: "Creator & Agency FP&A · AI build", requested_by: "Rosa Lind", due_at: "2020-01-01T00:00:00Z" },
  { ...base, id: "t-3", instance_id: "i-3", step_def_id: "b-submit", step_name: "Submit the budget", workflow_name: "Budget round",
    model_name: "Restaurant FP&A", requested_by: "Theo Marchetti", rework_count: 1, rework_note: 'Sent back for rework (1) from "Review": add the commentary' },
];

test("inbox: each task names its model, and the search matches every word against what the card shows", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/tasks", (route) => route.fulfill(json(tasks)));
  await page.route("**/api/workflow/definitions", (route) => route.fulfill(json([])));

  await loadAs(page, "dept_head");
  await page.getByRole("button", { name: "Workflow Inbox", exact: true }).click();
  await expect(page.getByText("Load GL postings")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("Model: Restaurant FP&A")).toBeVisible();
  await expect(page.getByText("3 of 3")).toBeVisible();

  const search = page.getByRole("searchbox", { name: "Search tasks" }).or(page.getByLabel("Search tasks"));
  const shows = async (query: string, names: string[]) => {
    await search.fill(query);
    await expect(page.getByText(`${names.length} of 3`)).toBeVisible();
    for (const n of ["Load GL postings", "Approve the monthly pack", "Submit the budget"]) {
      await expect(page.getByText(n, { exact: true })).toHaveCount(names.includes(n) ? 1 : 0);
    }
  };
  await shows("ai build", ["Approve the monthly pack"]);                       // model
  await shows("monthly close", ["Load GL postings", "Approve the monthly pack"]); // workflow
  await shows("overdue", ["Approve the monthly pack"]);                          // state
  await shows("sent back", ["Submit the budget"]);                               // rework
  await shows("theo close", ["Load GL postings"]);                               // every word: requester and workflow
  await shows("approval rosa", ["Approve the monthly pack"]);                    // step type and requester
  await search.fill("nothing like this");
  await expect(page.getByText(/No task matches/)).toBeVisible();
});
