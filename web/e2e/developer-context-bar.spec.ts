/**
 * The developer header must say WHICH application and model the selected
 * revision belongs to. Many models have a revision called "Working", and with
 * nothing selected the console falls back to a default revision that may be
 * another application's; a badge that only read "Working" once let objects be
 * created in the wrong model without any visible sign.
 */
import { test, expect } from "@playwright/test";
import { loadAs, mockApi } from "./mocks";

test("developer context bar names the application and model beside the revision badge", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");
  const bar = page.locator(".mvx-context-bar");
  await expect(bar).toContainText("Model", { timeout: 15_000 });
  // From the mocked GET /api/developer/model: app_name · model_name.
  await expect(bar).toContainText("Planning · Finance Model");
  await expect(bar).toContainText("Revision");
});
