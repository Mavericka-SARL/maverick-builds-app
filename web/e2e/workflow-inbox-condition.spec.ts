/**
 * A condition step lands in an inbox only when the engine could not
 * evaluate it (its context input is missing). The card must say so and let
 * the person pick the branch — the engine routes a condition on the
 * decision "true"/"false"; the generic "Complete" button used to send
 * "complete", which routed nowhere and closed the instance silently
 * (found by the 2026-09-13 workflow scenario run). Approval and task cards
 * keep their own controls. Mocked: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test.use({ actionTimeout: 10_000 });

const base = { workflow_name: "Stall Case", context: {}, status: "in_progress", created_at: "2026-09-13T10:00:00Z", required_comment: false, completion_label: "Complete", instructions: "" };
const tasks = [
  { ...base, id: "s-cond", instance_id: "i-1", step_def_id: "c-missing", step_name: "Over budget?", step_type: "condition",
    condition: { left: "amount", operator: "greater_than", right: 1000 } },
  { ...base, id: "s-appr", instance_id: "i-2", step_def_id: "appr", step_name: "Finance approval", step_type: "approval", workflow_name: "Expense Approval" },
  { ...base, id: "s-task", instance_id: "i-3", step_def_id: "book", step_name: "Auto-book", step_type: "task", completion_label: "Booked", workflow_name: "Expense Approval" },
  { ...base, id: "s-redo", instance_id: "i-4", step_def_id: "draft", step_name: "Draft", step_type: "task", workflow_name: "Rework", completion_label: "Resubmit",
    rework_count: 1, rework_note: 'Sent back for rework (1) from "Review": numbers are off' },
];

test("inbox: a stalled condition asks for the branch; approval and task keep their controls", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/tasks", (route) => route.fulfill(json(tasks)));
  const decisions: string[] = [];
  await page.route("**/api/tasks/*/complete", (route) => {
    const body = route.request().postDataJSON() as { decision: string };
    decisions.push(`${route.request().url().split("/").at(-2)}:${body.decision}`);
    return route.fulfill(json({ status: "ok" }));
  });
  await page.route("**/api/workflow/definitions", (route) => route.fulfill(json([])));

  await loadAs(page, "dept_head");
  await page.getByRole("button", { name: "Workflow Inbox", exact: true }).click();
  await expect(page.getByText("Over budget?")).toBeVisible({ timeout: 15_000 });

  // Condition card: explanation naming the missing input, branch buttons, no generic Complete.
  await expect(page.getByText(/could not be evaluated automatically/)).toBeVisible();
  await expect(page.getByText("amount", { exact: true }).first()).toBeVisible();
  await expect(page.getByRole("button", { name: "Continue as true" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Continue as false" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Complete", exact: true })).toHaveCount(0);
  // Approval and task cards keep their own controls.
  await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(1);
  await expect(page.getByRole("button", { name: "Reject" })).toHaveCount(1);
  await expect(page.getByRole("button", { name: "Booked" })).toHaveCount(1);

  // A step sent back by a later decision says so, with the reason.
  await expect(page.getByText("Sent back for rework (1)")).toBeVisible();
  await expect(page.getByText(/from "Review": numbers are off/)).toBeVisible();

  await page.getByRole("button", { name: "Continue as false" }).click();
  await expect.poll(() => decisions.join(","), { timeout: 10_000 }).toBe("s-cond:false");
});
