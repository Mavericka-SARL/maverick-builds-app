/**
 * The developer manual's screenshots, captured from a live console as the
 * reference demo's developer (cmd/seed-regional-planning). Each test leaves
 * one or more PNGs in docs/developer-manual/img; rebuild the manual after.
 * See playwright.manual.config.ts for what must be running.
 */
import { test, expect, type Page } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

const IMG = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../docs/developer-manual/img");
const DEVELOPER = "admin-created-sam.developer@meridian-industries.demo";

async function openConsole(page: Page, persona = DEVELOPER) {
  await page.addInitScript((p) => {
    if (!sessionStorage.getItem("manual-init")) {
      localStorage.clear();
      localStorage.setItem("dev_persona", p);
      sessionStorage.setItem("manual-init", "1");
    }
  }, persona);
  await page.goto("/");
  await expect(page.locator(".mvx-sidebar-nav")).toBeVisible();
  // The context bar names the revision once the model has loaded; the
  // console re-renders on the way there, which would undo an early click.
  await expect(page.getByText("Revision 1").first()).toBeVisible();
  await settle(page);
}

async function settle(page: Page) {
  await page.waitForLoadState("networkidle");
  await page.waitForTimeout(400);
}

async function tab(page: Page, label: string) {
  const item = page.locator(".mvx-sidebar-nav").getByRole("button", { name: label, exact: true });
  await item.click();
  await expect(item).toHaveAttribute("aria-current", "page");
  await settle(page);
}

/** The content pane beside the sidebar, as the manual frames most pictures. */
async function shotMain(page: Page, file: string) {
  await page.mouse.move(1399, 899); // no hover state in the picture
  const box = await page.locator(".mvx-app-shell__main").boundingBox();
  if (!box) throw new Error("no main pane");
  await page.screenshot({ path: path.join(IMG, file), clip: { x: box.x, y: 0, width: 1400 - box.x, height: 900 } });
}

async function shotFull(page: Page, file: string) {
  await page.mouse.move(1399, 899);
  await page.screenshot({ path: path.join(IMG, file) });
}

test("console tabs", async ({ page }) => {
  await openConsole(page);
  const tabs: [string, string][] = [
    ["Models", "01-models.png"],
    ["Metrics", "02-metrics.png"],
    ["Dimensions", "03-dimensions.png"],
    ["Forms", "04-forms.png"],
    ["Dashboards", "06-dashboards.png"],
    ["Workflows", "07-workflows.png"],
    ["Triggers", "08-triggers.png"],
    ["Integrations", "09-integrations.png"],
    ["AI Developer", "11-ai-developer.png"],
    ["Users", "12-users.png"],
    ["Dependency Graph", "37-dependency-graph.png"],
  ];
  for (const [label, file] of tabs) {
    await tab(page, label);
    await shotMain(page, file);
  }
  await tab(page, "Users");
  await page.getByRole("button", { name: /^Account:/ }).click();
  await settle(page);
  await shotFull(page, "13-account-menu.png");
});

const main = (page: Page) => page.locator(".mvx-app-shell__main");

test("models, metrics and dimensions", async ({ page }) => {
  await openConsole(page);

  await tab(page, "Metrics");
  await main(page).getByRole("button", { name: "Edit metric total_department_cost" }).click();
  await main(page).getByRole("button", { name: "Cancel" }).first().evaluate((el) => el.scrollIntoView({ block: "center" }));
  await settle(page);
  await shotMain(page, "21-metric-edit-inline.png");

  await openConsole(page);
  await tab(page, "Metrics");
  await main(page).getByRole("button", { name: "Add metric" }).click();
  await main(page).getByPlaceholder("e.g. gross_margin").fill("travel_share");
  await main(page).getByPlaceholder("e.g. Gross Margin %").fill("Travel share");
  const formula = main(page).getByPlaceholder("e.g. revenue - cost");
  await formula.fill("travel_cost / total_department_cost");
  await formula.evaluate((el) => el.scrollIntoView({ block: "center" }));
  await settle(page);
  await shotMain(page, "20-metric-add-form.png");

  await tab(page, "Dimensions");
  await main(page).getByRole("button", { name: "Expand department" }).click();
  await settle(page);
  await shotMain(page, "03-dimensions.png");
  await main(page).getByRole("button", { name: "Edit member NA Engineering" }).click();
  await settle(page);
  await shotMain(page, "22-dimension-member-edit.png");

  await tab(page, "Dependency Graph");
  // Two steps out show every metric of the demo at once.
  await main(page).getByRole("button", { name: "Zoom out" }).click();
  await main(page).getByRole("button", { name: "Zoom out" }).click();
  await settle(page);
  await shotMain(page, "37-dependency-graph.png");
});

test("forms and grids", async ({ page }) => {
  await openConsole(page);
  await tab(page, "Forms");
  await main(page).getByRole("button", { name: "Edit" }).first().click();
  await settle(page);
  await shotMain(page, "23-form-editor.png");

  await tab(page, "Grids");
  await main(page).getByRole("button", { name: "Configure" }).first().click();
  await settle(page);
  await shotMain(page, "24-grid-configure.png");
});

test("dashboards", async ({ page }) => {
  await openConsole(page);
  await tab(page, "Dashboards");
  await main(page).getByRole("button", { name: "Design" }).first().click();
  await settle(page);
  await shotMain(page, "25-dashboard-design.png");

  await main(page).getByText(/CHART\s*Regional Rollup/).first().click();
  await settle(page);
  await shotMain(page, "26-dashboard-widget-properties.png");

  await main(page).getByRole("button", { name: /^Preview$/ }).first().click();
  await settle(page);
  await page.waitForTimeout(2500);
  await shotMain(page, "28-dashboard-preview.png");

  await openConsole(page);
  await tab(page, "Dashboards");
  await main(page).getByRole("button", { name: "Design" }).first().click();
  await settle(page);
  await main(page).getByRole("button", { name: /^Chart$/ }).first().click();
  await main(page).getByLabel("Grid (source)").selectOption({ label: "Regional Rollup" });
  await settle(page);
  await shotMain(page, "27-dashboard-add-chart.png");
});

test("workflows and triggers", async ({ page }) => {
  await openConsole(page);
  await tab(page, "Workflows");
  await main(page).getByText("Travel Expense Approval", { exact: true }).click();
  await settle(page);
  await shotMain(page, "29-workflow-editor.png");

  await page.getByText(/^Manager Approval$/).first().click();
  await settle(page);
  await shotMain(page, "30-workflow-step-properties.png");

  await page.getByRole("button", { name: "Validate" }).click();
  await settle(page);
  await shotMain(page, "31-workflow-validate.png");

  await openConsole(page);

  await tab(page, "Triggers");
  await main(page).getByRole("button", { name: "New rule" }).click();
  await settle(page);
  await shotMain(page, "32-trigger-new-rule.png");
});

test("integrations", async ({ page }) => {
  await openConsole(page);
  await tab(page, "Integrations");
  await main(page).getByRole("button", { name: "New Import" }).click();
  await settle(page);
  await shotMain(page, "33-import-wizard.png");

  await tab(page, "Integrations");
  await main(page).getByText("Google Sheets", { exact: true }).first().click();
  await settle(page);
  await main(page).getByRole("button", { name: "Import from Sheet" }).click();
  await settle(page);
  await shotMain(page, "35-integrations-google-sheets.png");

  await tab(page, "Integrations");
  await main(page).getByText("REST API", { exact: true }).first().click();
  await settle(page);
  await main(page).getByRole("button", { name: "New integration" }).first().click();
  await settle(page);
  await shotMain(page, "34-integrations-rest-api.png");

  await tab(page, "Integrations");
  await main(page).getByText("Form Records", { exact: true }).first().click();
  await settle(page);
  await main(page).getByRole("button", { name: "New integration" }).first().click();
  await settle(page);
  await shotMain(page, "36-integrations-form-records.png");
});
