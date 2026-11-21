/**
 * The Start dialog opens with each variable's default filled in — the week
 * the plan starts on, a note built from it — and the person may change it
 * before starting (2026-11-21: a weekly replenishment cycle a schedule also
 * starts, with the same defaults). A default that cannot be worked out
 * leaves its field empty with the reason.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

test("start dialog: variables open filled in with their defaults", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/tasks", (route) => route.fulfill(json([])));
  await page.route("**/api/workflow/definitions", (route) => route.fulfill(json([{
    id: "wf-cycle", name: "Weekly cycle", description: "", trigger_event: "manual", may_start: true,
    context_schema: [
      { key: "week", label: "Week", data_type: "Dimension member", dimension_id: "d-week", has_default: true },
      { key: "note", label: "Note", data_type: "Text", has_default: true },
      { key: "qty", label: "Quantity", data_type: "Number", has_default: true },
    ],
  }])));
  await page.route("**/api/workflow/definitions/wf-cycle/start-defaults", (route) => route.fulfill(json({
    values: { week: "W2", note: "Weekly cycle W2" },
    errors: { qty: "it evaluates to #DIV/0!: division by zero" },
  })));
  await page.route("**/api/dimensions", (route) => route.fulfill(json([{
    id: "d-week", name: "week", members: [
      { id: "m-w1", code: "W1", label: "Week of 16 Nov" },
      { id: "m-w2", code: "W2", label: "Week of 23 Nov" },
    ],
  }])));
  let started: Record<string, string> | undefined;
  await page.route("**/api/workflow/instances", async (route) => {
    started = (route.request().postDataJSON() as { context: Record<string, string> }).context;
    await route.fulfill(json({ instance_id: "i-1" }));
  });

  await loadAs(page, "dept_head");
  await page.getByRole("button", { name: "Workflow", exact: true }).click();
  await page.getByRole("button", { name: "Start", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Start: Weekly cycle")).toBeVisible({ timeout: 15_000 });

  await expect(dialog.getByText("Week of 23 Nov")).toBeVisible();
  await expect(dialog.getByRole("textbox").first()).toHaveValue("Weekly cycle W2");
  await expect(dialog.getByText("Its default could not be worked out: it evaluates to #DIV/0!")).toBeVisible();

  // The person changes the note and fills the quantity; the week stays.
  await dialog.getByRole("textbox").first().fill("Catch-up week");
  await dialog.getByRole("spinbutton").fill("3");
  await dialog.getByRole("button", { name: "Submit", exact: true }).click();
  await expect.poll(() => started).toEqual({ week: "W2", note: "Catch-up week", qty: "3" });
});
