/**
 * A revision is renamed in its row in Developer Models (2026-11-08): the
 * AI-built application's header read "AI Draft 2026-11-08 10:12:55.576" to
 * every business user, and nothing renamed a revision.
 *
 * Mocked API (CI runs the e2e job without a gateway).
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

async function mockModel(page: Page, live: () => string) {
  await mockApi(page);
  await page.route("**/api/developer/applications*", (route) => route.fulfill(json([{
    id: "t-1", name: "Acme", plan: "test", created_at: "2026-01-01T00:00:00Z",
    applications: [{
      id: "app-1", name: "Planning", mode: "planning", status: "active",
      models: [{
        id: "model-1", name: "In Order Planning", storage_type: "oltp", active_revision: live(), is_default: true,
        revisions: [
          { id: "rev-live", name: live(), created_at: "2026-11-08T00:00:00Z" },
          { id: "rev-other", name: "Scratch", created_at: "2026-11-08T00:00:00Z" },
        ],
      }],
    }],
  }])));
}

test("the live revision is renamed in its row and keeps its Live badge", async ({ page }) => {
  let live = "AI Draft 2026-11-08 10:12:55.576";
  const sent: unknown[] = [];
  await mockModel(page, () => live);
  await page.route("**/api/developer/revisions/rev-live", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    const body = route.request().postDataJSON() as { name: string };
    sent.push(body);
    live = body.name;
    return route.fulfill(json({ id: "rev-live", name: body.name }));
  });

  await loadAs(page, "developer");
  const model = page.locator(".mvx-admin-model", { hasText: "In Order Planning" });
  await expect(model).toBeVisible({ timeout: 15_000 });
  await model.getByRole("button", { name: "Rename revision AI Draft 2026-11-08 10:12:55.576" }).click();
  const field = model.getByRole("textbox", { name: "New name of revision AI Draft 2026-11-08 10:12:55.576" });
  await field.fill("Production FP&A");
  await field.press("Enter");

  expect(sent).toEqual([{ name: "Production FP&A" }]);
  const row = model.locator(".mvx-admin-revision", { hasText: "Production FP&A" });
  await expect(row).toBeVisible();
  await expect(row.getByText(/^live$/i).first()).toBeVisible();
  await expect(model.getByRole("alert")).toHaveCount(0);
});

test("a name another revision has is refused and says why", async ({ page }) => {
  await mockModel(page, () => "Plan live");
  await page.route("**/api/developer/revisions/rev-other", (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    return route.fulfill(json({ error: "another revision of this model already has that name" }, 409));
  });

  await loadAs(page, "developer");
  const model = page.locator(".mvx-admin-model", { hasText: "In Order Planning" });
  await expect(model).toBeVisible({ timeout: 15_000 });
  await model.getByRole("button", { name: "Rename revision Scratch" }).click();
  await model.getByRole("textbox", { name: "New name of revision Scratch" }).fill("Plan live");
  await model.getByRole("button", { name: "Save" }).click();

  await expect(model.getByRole("alert")).toHaveText("Not renamed: 409: another revision of this model already has that name");
  await expect(model.getByRole("textbox", { name: "New name of revision Scratch" })).toBeVisible();
});
