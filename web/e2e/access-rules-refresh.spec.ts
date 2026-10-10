/**
 * Saving access rules refreshes what the console shows from the model — a
 * grid, a KPI, a chart — rather than leaving it to the 30-second refresh: an
 * admin who changed their own rules went on seeing the old ones. Going back
 * to a dashboard after the save reads the grid's structure — which rows and
 * metrics the viewer sees — again; its cells poll every 2 s regardless.
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("saving access rules makes a dashboard read its grid's structure again", async ({ page }) => {
  await mockApi(page);
  let gridReads = 0; // grid-meta: what the viewer may see
  page.on("request", (r) => {
    const url = new URL(r.url());
    if (url.pathname === "/api/grid" && url.searchParams.get("meta_only") === "1") gridReads++;
  });
  await loadAs(page, "finance");
  const nav = page.getByRole("navigation", { name: "Primary" });

  await nav.getByRole("button", { name: "Dashboards", exact: true }).click();
  await page.getByRole("button", { name: "OPEX 2" }).first().click();
  await expect.poll(() => gridReads, { timeout: 15_000 }).toBeGreaterThan(0);

  await nav.getByRole("button", { name: "Access Rules", exact: true }).click();
  await page.getByLabel("Select user").selectOption("u-1");
  // Each dimension's rules start collapsed.
  await page.getByRole("button", { name: "Expand all", exact: true }).click();
  await page.getByLabel("Access level").first().selectOption("read");
  const saved = page.waitForRequest((r) => r.method() === "PUT" && r.url().includes("/access-rules"));
  await page.getByRole("button", { name: "Save rules" }).click();
  await saved;
  const before = gridReads;

  // Back well inside the 30 s the grid would otherwise count as fresh.
  await nav.getByRole("button", { name: "Dashboards", exact: true }).click();
  await page.getByRole("button", { name: "OPEX 2" }).first().click();
  await expect.poll(() => gridReads, { timeout: 10_000 }).toBeGreaterThan(before);
});
