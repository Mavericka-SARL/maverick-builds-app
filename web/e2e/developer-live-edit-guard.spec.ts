/**
 * Build works in whichever revision is selected. With the live one selected,
 * the first change in a session asks first: business users see it at once
 * (2026-10-26). The header says "(live)"; Cancel leaves the revision as it
 * is; a yes holds for the rest of the session.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
const LIVE = { id: "rev-live", name: "Live", created_at: "2026-09-01T00:00:00Z", is_active: true };

test("the first change to the live revision asks first, and a yes holds for the session", async ({ page }) => {
  const patches: string[] = [];
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([LIVE])));
  await page.route("**/api/developer/metrics/m1*", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    patches.push(route.request().postData() ?? "");
    return route.fulfill(json({ status: "ok" }));
  });

  await loadAs(page, "developer");
  await expect(page.getByText("Live (live)")).toBeVisible({ timeout: 15_000 });
  await page.getByRole("button", { name: "Metrics", exact: true }).click();

  // Cancel: nothing is sent, and the developer is told why.
  await page.getByRole("button", { name: "Edit metric revenue" }).click();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  const ask = page.getByRole("alertdialog");
  await expect(ask.getByText("Edit the live revision?")).toBeVisible();
  await expect(ask).toContainText('"Live" is the revision business users work in now');
  await ask.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByText(/Not changed: you chose to leave the live revision as it is/)).toBeVisible();
  expect(patches).toHaveLength(0);

  // Yes: the change is sent.
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Edit the live revision" }).click();
  await expect.poll(() => patches.length).toBe(1);

  // The next change in the session does not ask again.
  await page.getByRole("button", { name: "Edit metric revenue" }).click();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => patches.length).toBe(2);
  await expect(page.getByRole("alertdialog")).toHaveCount(0);
});

test("a revision that is not live is changed without asking", async ({ page }) => {
  const patches: string[] = [];
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ ...LIVE, is_active: false, name: "Next year" }])));
  await page.route("**/api/developer/metrics/m1*", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    patches.push(route.request().postData() ?? "");
    return route.fulfill(json({ status: "ok" }));
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Metrics", exact: true }).click();
  await page.getByRole("button", { name: "Edit metric revenue" }).click();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => patches.length).toBe(1);
  await expect(page.getByRole("alertdialog")).toHaveCount(0);
  await expect(page.getByText("(live)")).toHaveCount(0);
});
