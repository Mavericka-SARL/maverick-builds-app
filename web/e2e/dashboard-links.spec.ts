/**
 * A text widget links to another dashboard by name
 * (consoles/business/dashboardLinks): one of the same model opens in
 * place; one of another model reloads the console into that model and
 * opens the dashboard it names, not merely the model's first. A target the
 * reader cannot open is reported, never guessed at.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const text = (id: string, content: string) => ({
  id, widget_type: "text", ref_id: null, sort_order: 0, col_start: 1, col_span: 12,
  pos_x: 0, pos_y: 0, size_w: 600, size_h: 200, content,
});

// Model 1 is the default (the mocks' context); model 2 is a guide beside it.
const byModel: Record<string, { id: string; name: string; tags: string[]; folder_id: null; widgets: unknown[] }[]> = {
  "model-1": [
    {
      id: "m1-home", name: "Home", tags: [], folder_id: null,
      widgets: [text("t1", [
        "Same model: [the numbers](dashboard:Numbers).",
        "Another model: [the guide's second page](dashboard:Guide model/2 · Start).",
        "Gone: [nowhere](dashboard:No such model/Anything).",
      ].join("\n\n"))],
    },
    { id: "m1-numbers", name: "Numbers", tags: [], folder_id: null, widgets: [text("t2", "These are the numbers.")] },
  ],
  "model-2": [
    { id: "m2-intro", name: "1 · Intro", tags: [], folder_id: null, widgets: [text("t3", "The guide's first page.")] },
    { id: "m2-start", name: "2 · Start", tags: [], folder_id: null, widgets: [text("t4", "Welcome to the guide's second page.")] },
  ],
};

async function twoModels(page: Page) {
  await mockApi(page);
  const modelOf = (headers: Record<string, string>) => headers["x-model-id"] || "model-1";
  await page.route("**/api/apps", route => route.fulfill({
    json: [{
      id: "app-1", name: "Getting started", workspace_name: "Main", model_name: "Finance Model",
      models: [
        { id: "model-1", name: "Finance Model", is_default: true, active_revision: "FY2026 Budget" },
        { id: "model-2", name: "Guide model", is_default: false, active_revision: "First revision" },
      ],
    }],
  }));
  await page.route("**/api/dashboards", route => route.fulfill({ json: byModel[modelOf(route.request().headers())] }));
  await page.route("**/api/dashboards/*", route => {
    const id = new URL(route.request().url()).pathname.split("/").pop();
    const all = Object.values(byModel).flat();
    return route.fulfill({ json: all.find(d => d.id === id) ?? all[0] });
  });
  // The console works in whichever model the client asks for.
  await page.route("**/api/demo", route => route.fulfill({
    json: {
      app_id: "app-1", model_id: modelOf(route.request().headers()), revision_id: "rev-1", revision: "FY2026 Budget",
      actor: { user_id: "u-1", email: "alex@example.com", display_name: "Alex", roles: ["business_user"] },
    },
  }));
}

async function openDashboards(page: Page) {
  await loadAs(page, "business_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await expect(page.getByText("Same model:")).toBeVisible();
}

test("a link to a dashboard of the same model opens it in place", async ({ page }) => {
  await twoModels(page);
  await openDashboards(page);
  await page.getByRole("link", { name: "the numbers" }).click();
  await expect(page.getByText("These are the numbers.")).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem("selected_model_id"))).toBeNull();
});

test("a link to another model's dashboard opens that model at that dashboard", async ({ page }) => {
  await twoModels(page);
  await openDashboards(page);
  await Promise.all([
    page.waitForEvent("load"),
    page.getByRole("link", { name: "the guide's second page" }).click(),
  ]);
  await expect(page.getByText("Welcome to the guide's second page.")).toBeVisible();
  await expect(page.locator(".mvx-context-bar")).toContainText("Getting started · Guide model");
  await expect(page.getByText("The guide's first page.")).toHaveCount(0);
  expect(await page.evaluate(() => localStorage.getItem("selected_model_id"))).toBe("model-2");

  // Asked once: a later reload opens the model as usual, at its first page.
  await page.reload();
  await expect(page.getByText("The guide's first page.")).toBeVisible();
});

test("a link to a model the reader cannot open says so", async ({ page }) => {
  await twoModels(page);
  await openDashboards(page);
  await page.getByRole("link", { name: "nowhere" }).click();
  await expect(page.getByText("No model named “No such model” is open to you.")).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem("selected_model_id"))).toBeNull();
});
