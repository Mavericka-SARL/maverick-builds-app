/**
 * The workflow diagram names a route's target step, not its internal id
 * (2026-11-19, OPEX Budget 2027 "Budget approval": the first approval read
 * "Approve → step-nxr65fs" where the step list says "CFO approval").
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const WF_ID = "wf-budget";
const wf = {
  id: WF_ID, application_id: "app-1", name: "Budget approval", description: "", trigger_event: "manual",
  subject_type: "", subject_config: {}, status: "draft", context_schema: [],
  created_at: "2026-11-19T00:00:00Z", updated_at: "2026-11-19T00:00:00Z", published_at: null, archived_at: null,
  steps: [
    { id: "step-a1b2c3d", name: "Owner review", type: "approval", assignee_roles: ["business_admin"],
      routes: { approve: "step-nxr65fs", reject: "end-rejected" } },
    { id: "step-nxr65fs", name: "CFO approval", type: "approval", assignee_roles: ["business_admin"],
      routes: { approve: "step-k9cond0", reject: "step-a1b2c3d" } },
    { id: "step-k9cond0", name: "Within budget?", type: "condition", condition: { formula: "1 = 1" },
      routes: { true: "end-completed", false: "step-gone00" } },
  ],
};

test("routes name their target steps; ends read in words; an unknown id stays as it is", async ({ page }) => {
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await mockApi(page);
  await page.route("**/api/developer/workflows?*", (route) => route.fulfill(json([wf])));
  await page.route(`**/api/developer/workflows/${WF_ID}`, (route) => route.fulfill(json(wf)));
  await page.route(`**/api/developer/workflows/${WF_ID}/usage`, (route) => route.fulfill(json([])));
  await page.route(`**/api/developer/workflows/${WF_ID}/instances`, (route) => route.fulfill(json([])));
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Workflows", exact: true }).click();
  await page.getByRole("button", { name: "Budget approval", exact: true }).first().click();
  await expect(page.getByText("Approve → CFO approval")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("Reject → Rejected")).toBeVisible();
  await expect(page.getByText("Approve → Within budget?")).toBeVisible();
  await expect(page.getByText("Reject → Owner review")).toBeVisible();
  await expect(page.getByText("True → Completed")).toBeVisible();
  await expect(page.getByText("False → step-gone00")).toBeVisible();
  await expect(page.getByText(/→ step-nxr65fs/)).toHaveCount(0);
});
