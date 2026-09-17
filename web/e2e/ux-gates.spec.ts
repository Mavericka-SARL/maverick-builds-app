/**
 * Phase 6 UX regression gates (UX_UPGRADE_INSTRUCTIONS.md).
 *
 * Runs every role console against mocked APIs at the three target viewports
 * and asserts structural invariants of the design system:
 *  - the shared shell renders (sidebar nav + persona switcher),
 *  - the page never scrolls horizontally,
 *  - keyboard focus is visible on nav items,
 *  - confirm dialogs render as accessible alertdialogs and can be cancelled,
 *  - planning grid cells expose the shared editable/calc state classes,
 *  - console source files do not re-accumulate hard-coded hex colors.
 */
import { test, expect, type Page } from "@playwright/test";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { mockApi, loadAs } from "./mocks";

const VIEWPORTS = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "laptop", width: 1280, height: 800 },
  { name: "tablet", width: 768, height: 1024 },
];

const PERSONAS: { persona: string; screens: string[] }[] = [
  { persona: "dept_head", screens: ["Dashboards", "Workflow Inbox", "My History", "Models"] },
  { persona: "finance", screens: ["Dashboards", "Workflow Inbox", "Forms", "History", "Roles", "Access Rules"] },
  { persona: "developer", screens: ["Models", "Metrics", "Dimensions", "Grids", "Dashboards", "Triggers", "Integrations"] },
  { persona: "platform_admin", screens: ["Applications", "Users", "Audit Log"] },
];

async function assertNoHorizontalScroll(page: Page, context: string) {
  const overflow = await page.evaluate(() =>
    document.documentElement.scrollWidth - document.documentElement.clientWidth
  );
  expect(overflow, `page overflows horizontally on ${context}`).toBeLessThanOrEqual(1);
}

for (const vp of VIEWPORTS) {
  for (const { persona, screens } of PERSONAS) {
    test(`${persona} console at ${vp.name} (${vp.width}x${vp.height})`, async ({ page }) => {
      await page.setViewportSize({ width: vp.width, height: vp.height });
      await mockApi(page);
      await loadAs(page, persona);

      // Shared shell present
      await expect(page.getByRole("navigation", { name: "Primary" })).toBeVisible();
      await expect(page.getByLabel("Switch persona")).toBeVisible();

      for (const screen of screens) {
        await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: screen, exact: true }).click();
        await page.waitForTimeout(150);
        await assertNoHorizontalScroll(page, `${persona}/${screen} @ ${vp.name}`);
      }
    });
  }
}

test("keyboard focus ring is visible on nav items", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");

  const firstNav = page.getByRole("navigation", { name: "Primary" }).getByRole("button").first();
  await firstNav.focus();
  const focusStyle = await firstNav.evaluate((el) => {
    const s = getComputedStyle(el);
    return { outlineStyle: s.outlineStyle, outlineWidth: s.outlineWidth, boxShadow: s.boxShadow };
  });
  const hasVisibleFocus =
    (focusStyle.outlineStyle !== "none" && focusStyle.outlineWidth !== "0px") ||
    focusStyle.boxShadow !== "none";
  expect(hasVisibleFocus, `focus must be visible, got ${JSON.stringify(focusStyle)}`).toBe(true);
});

test("confirm dialog is an accessible alertdialog and cancels", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "platform_admin");

  // Deleting a model opens the shared ConfirmDialog.
  await page.getByRole("button", { name: /Delete model/ }).first().click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Cancel" })).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).not.toBeVisible();
});

test("planning grid exposes shared editable and calc cell states", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "dept_head");

  // The mocked "OPEX 2" dashboard embeds the planning grid widget.
  await expect(page.getByText("OPEX 2")).toBeVisible();
  // With Region=Global (an aggregate member) every cell is readonly; pick a
  // leaf member so input metrics become editable. The context selector is a
  // custom popover (HierarchicalMemberSelect), not a native <select>, so
  // this is click-driven rather than selectOption — same aria-label though.
  await page.getByLabel("Region context").click();
  await page.getByRole("option", { name: "EMEA" }).click();
  await expect(page.locator(".mvx-cell-input").first()).toBeVisible();
  await expect(page.locator(".mvx-cell--calc").first()).toBeVisible();
});

test("import wizard uses the shared stepper and dropzone", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");

  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "New Import" }).click();
  await expect(page.locator(".mvx-stepper")).toBeVisible();
  await expect(page.locator(".mvx-dropzone")).toBeVisible();
});

test("AI developer renders empty state inside the shell", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "developer");

  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "AI Developer" }).click();
  await expect(page.getByText("AI Developer", { exact: true }).first()).toBeVisible();
  await expect(page.getByRole("button", { name: "New session" })).toBeVisible();
});

// ── Hard-coded color guardrail ────────────────────────────────────────────────
// Consoles must not re-accumulate hex colors; the design system owns color.
// Budgets are the counts at the time Phase 6 landed — lower them as files are
// cleaned further, never raise them without a design-system discussion.

const HEX_BUDGETS: Record<string, number> = {
  // BusinessConsole.tsx was split into domain-module sibling files
  // (2026-08-05), mirroring the DeveloperConsole.tsx split below — every
  // file in this family is now zero hex, no <input type="color"> exception
  // needed here (unlike DashboardCanvas.tsx).
  "consoles/business/BusinessConsole.tsx": 0,
  "consoles/business/PlanningGrid.tsx": 0,
  "consoles/business/WorkflowInboxTab.tsx": 0,
  "consoles/business/FormsTab.tsx": 0,
  "consoles/business/DashboardsView.tsx": 0,
  "consoles/business/DashboardWidgets.tsx": 0,
  "consoles/business/ImportWidget.tsx": 0,
  "consoles/business/WorkflowHistoryTab.tsx": 0,
  "consoles/business/AppsTab.tsx": 0,
  "consoles/business/blobUtils.ts": 0,
  // DeveloperConsole.tsx was split into domain-module sibling files
  // (2026-08-04) — the shell itself now has zero hex colors, same as every
  // new file below except DashboardCanvas.tsx, whose remaining 6 are a
  // documented exception (see comment there): native <input type="color">
  // value attributes can't accept a CSS var(), only a literal hex string.
  "consoles/developer/DeveloperConsole.tsx": 0,
  "consoles/developer/ApplicationsTab.tsx": 0,
  "consoles/developer/FormsTab.tsx": 0,
  "consoles/developer/AutomationTab.tsx": 0,
  "consoles/developer/GridsTab.tsx": 0,
  "consoles/developer/DimensionsTab.tsx": 0,
  "consoles/developer/MetricsTab.tsx": 0,
  "consoles/developer/DependencyGraphTab.tsx": 0,
  "consoles/developer/IntegrationsTab.tsx": 0,
  "consoles/developer/DashboardsTab.tsx": 0,
  "consoles/developer/DashboardCanvas.tsx": 6, // color-picker native defaults, see file
  // WorkflowsTab.tsx was split into domain-module sibling files (2026-08-09),
  // the same treatment as DeveloperConsole.tsx/BusinessConsole.tsx above —
  // every file in this family is now zero hex, no <input type="color">
  // exception needed here either.
  "consoles/developer/WorkflowsTab.tsx": 0,
  "consoles/developer/WorkflowShared.tsx": 0,
  "consoles/developer/workflowConstants.ts": 0,
  "consoles/developer/CreateAutomationModal.tsx": 0,
  "consoles/developer/StepPropertiesPanel.tsx": 0,
  "consoles/developer/WorkflowPropertiesPanel.tsx": 0,
  "consoles/developer/WorkflowCanvas.tsx": 0,
  "consoles/developer/WorkflowEditor.tsx": 0,
  "consoles/developer/ImportWizard.tsx": 0,
  "consoles/developer/AIAssistant.tsx": 0,
  "consoles/business-admin/BusinessAdminConsole.tsx": 0,
  "consoles/platform-admin/PlatformAdminConsole.tsx": 0,
  "consoles/admin/UsersPanel.tsx": 0,
  "consoles/dashboard/chartTypes.ts": 10, // approved data-viz palette
  "consoles/dashboard/ChartWidgetEditor.tsx": 6,
  "consoles/dashboard/ChartWidget.tsx": 0,
};
const DEFAULT_BUDGET = 5; // any console file not listed above

test("console files stay within hard-coded hex color budgets", () => {
  const here = dirname(fileURLToPath(import.meta.url));
  const srcRoot = join(here, "..", "src");
  const consolesRoot = join(srcRoot, "consoles");

  const files: string[] = [];
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) walk(full);
      else if (/\.(tsx?|css)$/.test(entry)) files.push(full);
    }
  };
  walk(consolesRoot);

  const failures: string[] = [];
  for (const file of files) {
    const rel = file.slice(srcRoot.length + 1);
    const count = (readFileSync(file, "utf8").match(/#[0-9a-fA-F]{3,8}\b/g) ?? []).length;
    const budget = HEX_BUDGETS[rel] ?? DEFAULT_BUDGET;
    if (count > budget) failures.push(`${rel}: ${count} hex colors (budget ${budget})`);
  }
  expect(failures, failures.join("\n")).toEqual([]);
});

// agg_rule "formula" (and "rate") cannot be derived from a metric's members:
// the first is the metric's own expression re-evaluated against aggregated
// inputs, the second one metric's total over another's. The browser has no
// formula evaluator — removed deliberately when grid values converged on
// server-computed ones — so it has to read the server's answer.
//
// resolveCell's switch had no case for either, so both fell to `default` and
// were summed. A Sales metric set to Formula reported 92 + 200 = 292 where its
// own rule said 729.
test("a formula-rule metric's total comes from the server, not from summing its members", async ({ page }) => {
  await mockApi(page);
  await loadAs(page, "dept_head");

  await expect(page.getByText("OPEX 2")).toBeVisible();
  const grid = page.locator("table").filter({ hasText: "Margin Pct" }).first();
  await expect(grid).toBeVisible({ timeout: 15_000 });

  // Members are 10 / 30 / 20; summing would show 60. The contract has
  // tightened since this test was written: g.totals is the WHOLE grid's
  // aggregate, blind to context selectors, so a rollup cell in a
  // context-pinned view shows "—" rather than any number that might be
  // false — a Canada-restricted user once saw the all-periods margin_pct
  // at a Q1 context because this cell displayed the grand total. The one
  // thing that must never appear is the member-sum (60): that is the
  // wrong-math regression this test originally caught, and it stays the
  // tripwire for any revert.
  const row = grid.locator("tr").filter({ hasText: "Margin Pct" }).first();
  await expect(row).toContainText("—");
  await expect(row).not.toContainText("60");
  await expect(row).not.toContainText("42");
});
