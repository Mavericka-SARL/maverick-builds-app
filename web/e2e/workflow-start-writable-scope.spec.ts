/**
 * The Start dialog offers only a scope the person may write as a whole: an
 * instance locks its member and everything under it, so a member they may
 * only read — or one with a read-only member under it — is not offered
 * (2026-10-25: a regional manager with Read on AMERICAS started its budget
 * approval and froze the Americas owner's numbers). The server refuses such
 * a start too (ResolveStartContext); the dialog is the person's guide.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

test("start dialog: only members the person may write in full are offered", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/tasks", (route) => route.fulfill(json([])));
  await page.route("**/api/workflow/definitions", (route) => route.fulfill(json([{
    id: "wf-budget", name: "Budget approval", description: "", trigger_event: "manual",
    context_schema: [{ key: "region", label: "Region", data_type: "Dimension member", dimension_id: "d-region" }],
  }])));
  await page.route("**/api/dimensions", (route) => route.fulfill(json([{
    id: "d-region", name: "region", members: [
      { id: "m-am", code: "AMERICAS", label: "Americas", access: "read" },
      { id: "m-us", code: "USA", label: "United States", parent_member_id: "m-am" },
      { id: "m-eu", code: "EUROPE", label: "Europe" },
      { id: "m-fr", code: "FRANCE", label: "France", parent_member_id: "m-eu", access: "read" },
      { id: "m-de", code: "GERMANY", label: "Germany", parent_member_id: "m-eu" },
    ],
  }])));

  await loadAs(page, "dept_head");
  await page.getByRole("button", { name: "Workflow Inbox", exact: true }).click();
  await page.getByRole("button", { name: "Start", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Start: Budget approval")).toBeVisible({ timeout: 15_000 });

  // Americas (read-only) and Europe (France under it is) cannot be picked:
  // they are shown as headings for the members under them, not options.
  await dialog.getByRole("button", { name: "Region" }).click();
  await expect(page.getByRole("option", { name: "United States" })).toHaveCount(1);
  await expect(page.getByRole("option", { name: "Germany" })).toHaveCount(1);
  await expect(page.getByRole("option", { name: "Americas" })).toHaveCount(0);
  await expect(page.getByRole("option", { name: "Europe" })).toHaveCount(0);
  await expect(page.getByRole("option", { name: "France" })).toHaveCount(0);
  await dialog.getByRole("button", { name: "Region" }).click(); // closes the list, not the dialog

  // Leaves the person may write: United States and Germany, not France.
  await dialog.getByRole("button", { name: "Select all (2)" }).click();
  await expect(dialog.getByText("United States")).toBeVisible();
  await expect(dialog.getByText("Germany")).toBeVisible();
  await expect(dialog.getByText("France")).toHaveCount(0);
});
