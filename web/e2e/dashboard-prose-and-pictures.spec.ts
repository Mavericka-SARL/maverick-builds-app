/**
 * Text and image widgets: what an explanatory dashboard is built from —
 * the sign-up tour (internal/starter) is written in exactly these. Text
 * reads a small Markdown subset, rendered as elements rather than as HTML;
 * an image carries its own picture as a data URL.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("a text widget renders headings, emphasis, lists and links", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "business_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "Explainer" }).first().click();

  await expect(page.getByRole("heading", { name: "How this works" })).toBeVisible();
  await expect(page.locator("strong", { hasText: "addressed" })).toBeVisible();
  await expect(page.getByRole("listitem")).toHaveCount(2);
  const link = page.getByRole("link", { name: "the handbook" });
  await expect(link).toHaveAttribute("href", "https://example.com/handbook");
  await expect(link).toHaveAttribute("rel", "noopener noreferrer");
});

test("an image widget shows its picture, with its alternative text", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "business_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "Explainer" }).first().click();

  const img = page.getByRole("img", { name: "A diagram of the model" });
  await expect(img).toBeVisible();
  // Decoded, not merely present: a broken data URL reports zero here.
  expect(await img.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBeGreaterThan(0);
  await expect(img).toHaveAttribute("src", /^data:image\/svg\+xml;base64,/);
});

test("markdown that is really a script stays text", async ({ page }) => {
  await mockApi(page);
  // A tenant's own prose is never trusted as markup: the renderer builds
  // elements, so an injected tag can only ever be shown, never run.
  await page.route("**/api/dashboards/dash-5", (route) =>
    route.fulfill({
      json: {
        id: "dash-5", name: "Explainer", tags: ["guide"], folder_id: null,
        widgets: [{
          id: "w5", widget_type: "text", ref_id: null, sort_order: 0, col_start: 1, col_span: 12,
          pos_x: 0, pos_y: 0, size_w: 600, size_h: 160,
          content: "<script>window.__pwned = true</script>\n\n[go](javascript:alert(1))",
        }],
      },
    }));
  await loadAs(page, "business_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "Explainer" }).first().click();

  await expect(page.getByText("window.__pwned = true")).toBeVisible();
  expect(await page.evaluate(() => (window as unknown as Record<string, unknown>).__pwned)).toBeUndefined();
  await expect(page.locator("script#nope")).toHaveCount(0);
  await expect(page.getByRole("link", { name: "go" })).toHaveCount(0);
});
