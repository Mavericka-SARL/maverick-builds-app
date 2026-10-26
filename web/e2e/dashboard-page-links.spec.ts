/**
 * A text widget links to a page of the console's sidebar by the names it
 * shows (router/pageLinks): "page:Workflow Inbox", or with its group
 * "page:Developer › Dimensions". The link opens the page as a click in the
 * sidebar would; a page the reader's roles do not give them is reported,
 * never opened.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const guide = {
  id: "guide", name: "1 Start here", tags: [], folder_id: null,
  widgets: [{
    id: "t1", widget_type: "text", ref_id: null, sort_order: 0, col_start: 1, col_span: 12,
    pos_x: 0, pos_y: 0, size_w: 800, size_h: 240,
    content: [
      "Your steps wait in [your inbox](page:Workflow Inbox).",
      "Who sees what: [the roles](page:business admin > roles).",
      "The structure: [the dimensions](page:Developer › Dimensions).",
      "Revisions: [the models](page:Developer › Models).",
      "Gone: [nowhere](page:No such page).",
    ].join("\n\n"),
  }],
};

async function withGuide(page: Page, roles?: string[]) {
  await mockApi(page);
  await page.route("**/api/dashboards", route => route.fulfill({ json: [guide] }));
  await page.route("**/api/dashboards/*", route => route.fulfill({ json: guide }));
  if (roles) {
    await page.route("**/api/me", route => route.fulfill({
      json: { user_id: "u-1", email: "sam@example.com", display_name: "Sam", roles },
    }));
  }
}

const nav = (page: Page) => page.getByRole("navigation", { name: "Primary" });

async function openGuide(page: Page) {
  await nav(page).getByRole("button", { name: "Dashboards" }).first().click();
  await expect(page.getByText("Your steps wait in")).toBeVisible();
}

test("a page link opens that page of the sidebar", async ({ page }) => {
  await withGuide(page);
  await loadAs(page, "finance"); // business admin + business user
  await openGuide(page);
  await page.getByRole("link", { name: "your inbox" }).click();
  await expect(nav(page).getByRole("button", { name: "Workflow Inbox" })).toHaveAttribute("aria-current", "page");

  // The group, in any case and with ">", picks the page among groups.
  await openGuide(page);
  await page.getByRole("link", { name: "the roles" }).click();
  await expect(nav(page).getByRole("button", { name: "Roles" })).toHaveAttribute("aria-current", "page");
});

test("a page the reader's roles do not give them is reported, not opened", async ({ page }) => {
  await withGuide(page);
  await loadAs(page, "dept_head"); // business user only
  await openGuide(page);
  await page.getByRole("link", { name: "the dimensions" }).click();
  await expect(page.getByText("“Developer › Dimensions” is not a page in your console.")).toBeVisible();
  await expect(page.getByText("Your steps wait in")).toBeVisible();
  await page.getByRole("link", { name: "nowhere" }).click();
  await expect(page.getByText("“No such page” is not a page in your console.")).toBeVisible();
});

test("a developer with the Business user role follows a link into the Developer group", async ({ page }) => {
  await withGuide(page, ["business_user", "developer"]);
  await loadAs(page, "developer");
  await openGuide(page);
  await page.getByRole("link", { name: "the dimensions" }).click();
  await expect(nav(page).getByRole("button", { name: "Dimensions" })).toHaveAttribute("aria-current", "page");
});

// Developer › Models became Developer Models (2026-10-26, beside the User
// group's Models): a link written with the former name still opens it.
test("a link naming a renamed page by its former name still opens it", async ({ page }) => {
  await withGuide(page, ["business_user", "developer"]);
  await loadAs(page, "developer");
  await openGuide(page);
  await page.getByRole("link", { name: "the models" }).click();
  await expect(nav(page).getByRole("button", { name: "Developer Models" })).toHaveAttribute("aria-current", "page");
});
