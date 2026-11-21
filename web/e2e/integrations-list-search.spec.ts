/**
 * Integrations › Excel / CSV: Saved Integrations and Recent Imports get a
 * search box and scroll in place once they list more than 10 (2026-11-21);
 * Recent Imports asks for the newest 200, not 20, so the search finds
 * older imports. 10 or fewer show as before.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });

const people = ["Anna", "Boris"];
const jobs = (n: number) => Array.from({ length: n }, (_, i) => ({
  id: `job-${i}`, model_id: "model-1", revision_name: "Working", status: 3, total_rows: 5, valid_rows: 5, error_rows: 0,
  created_at: `2026-11-${String(20 - (i % 20)).padStart(2, "0")}T10:00:00Z`, source: "upload", target_type: "grid",
  target_name: i === 7 ? "Recorded costs" : "Sales", file_name: `file-${i}.xlsx`, by: people[i % 2], duration_ms: 400,
}));
const integrations = (n: number) => Array.from({ length: n }, (_, i) => ({
  id: `int-${i}`, name: i === 4 ? "Weekly stock counts" : `Monthly sales ${i}`, type: "csv_import", target_type: "grid",
  target_id: "grid-1", status: "active", tags: i === 9 ? ["warehouse"] : [], config: { column_map: {} },
}));

async function open(page: Page, nJobs: number, nIntegrations: number) {
  const limits: string[] = [];
  await page.route("**/api/import/jobs?*", (route) => {
    limits.push(new URL(route.request().url()).searchParams.get("limit") ?? "");
    return route.fulfill(json(jobs(nJobs)));
  });
  await page.route("**/api/developer/integrations*", (route) =>
    route.request().method() === "GET" ? route.fulfill(json(integrations(nIntegrations))) : route.fallback());
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  return limits;
}

test.beforeEach(async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
});

test("more than 10: both lists search every word of what they show and scroll in place", async ({ page }) => {
  const limits = await open(page, 25, 12);

  // Recent Imports: all 25 listed in a box that scrolls, its header kept.
  const box = page.getByTestId("recent-imports");
  await expect(box.getByRole("row")).toHaveCount(26);
  expect(limits).toContain("200");
  const scrolls = await box.evaluate((el) => el.scrollHeight > el.clientHeight && getComputedStyle(el).overflowY === "auto");
  expect(scrolls).toBe(true);
  const search = page.getByRole("searchbox", { name: "Search recent imports" });
  await search.fill("recorded boris");
  await expect(box.getByRole("row")).toHaveCount(2);
  await expect(box.getByRole("row").nth(1)).toContainText("Recorded costs");
  await expect(page.getByText("1 of 25 imports")).toBeVisible();
  await search.fill("nothing like this");
  await expect(page.getByText("No import matches “nothing like this”.")).toBeVisible();
  await expect(box).toHaveCount(0);
  await search.fill("");
  await expect(box.getByRole("row")).toHaveCount(26);

  // Saved Integrations: name and tag found.
  const saved = page.getByTestId("saved-integrations");
  const find = page.getByRole("searchbox", { name: "Search saved integrations" });
  await find.fill("weekly stock");
  await expect(saved.getByText("Weekly stock counts")).toBeVisible();
  await expect(saved.getByText(/Monthly sales/)).toHaveCount(0);
  await find.fill("warehouse");
  await expect(saved.getByText("Monthly sales 9")).toBeVisible();
  await expect(page.getByText("1 of 12 integrations")).toBeVisible();
});

test("10 or fewer: no search box, no scrolling", async ({ page }) => {
  await open(page, 10, 3);
  await expect(page.getByTestId("recent-imports").getByRole("row")).toHaveCount(11);
  await expect(page.getByRole("searchbox", { name: "Search recent imports" })).toHaveCount(0);
  await expect(page.getByRole("searchbox", { name: "Search saved integrations" })).toHaveCount(0);
  const maxHeight = await page.getByTestId("recent-imports").evaluate((el) => getComputedStyle(el).maxHeight);
  expect(maxHeight).toBe("none");
});
