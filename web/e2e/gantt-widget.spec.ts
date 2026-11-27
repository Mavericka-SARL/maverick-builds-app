/**
 * The Gantt widget (GanttPanel.tsx): a table's records as bars on a
 * timeline, grouped, their baseline under them, a record without an end a
 * milestone, and an arrow from the record each follows — red when it starts
 * before that one ends (2026-11-27: the timeline used to be a grid of glyphs
 * and the dependencies were only checked).
 *
 * Self-contained: CI runs without a gateway.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const ACTIVITIES = {
  id: "acts", model_id: "m", name: "activities", label: "Activities", own_records_only: false, edit_access: "everyone",
  fields: [
    { name: "code", label: "Activity", type: "text", code: true },
    { name: "title", label: "Title", type: "text" },
    { name: "project", label: "Project", type: "text" },
    { name: "start", label: "Start", type: "date" },
    { name: "finish", label: "Finish", type: "date" },
    { name: "base_start", label: "Baseline start", type: "date" },
    { name: "base_finish", label: "Baseline finish", type: "date" },
    { name: "after", label: "Follows", type: "text" },
  ],
};
const rec = (id: string, data: Record<string, unknown>) => ({ id, form_id: "acts", data, status: "", created_by: null, created_at: "2027-01-01", updated_at: "2027-01-01" });
const records = [
  rec("r1", { code: "A1", title: "Design the clinic", project: "Clinic", start: "2027-01-10", finish: "2027-03-31", base_start: "2027-01-10", base_finish: "2027-02-28" }),
  rec("r2", { code: "A2", title: "Build the clinic", project: "Clinic", start: "2027-03-01", finish: "2027-08-31", after: "A1" }),
  rec("r3", { code: "M1", title: "Clinic opens", project: "Clinic", start: "2027-09-15" }),
  rec("r4", { code: "B1", title: "Train teachers", project: "School", start: "2027-04-01", finish: "2027-06-30" }),
];

async function serve(page: Page) {
  const dashboard = { id: "dash-gantt", name: "Timeline", tags: [], widgets: [{ id: "wg", widget_type: "gantt", ref_id: "acts", content: null, title: "Activity plan",
    sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 1100, size_h: 420,
    widget_props: { gantt: { start_field: "start", end_field: "finish", label_field: "title", group_field: "project",
      baseline_start_field: "base_start", baseline_end_field: "base_finish", predecessor_field: "after" } } }] };
  await page.route((url) => url.pathname === "/api/dashboards" || url.pathname.startsWith("/api/dashboards/"), (route) =>
    route.fulfill({ json: new URL(route.request().url()).pathname === "/api/dashboards" ? [dashboard] : dashboard }));
  await page.route((url) => url.pathname === "/api/forms", (route) => route.fulfill({ json: [ACTIVITIES] }));
  await page.route((url) => url.pathname === "/api/dimensions", (route) => route.fulfill({ json: [] }));
  await page.route((url) => url.pathname === "/api/forms/acts/records/query", (route) =>
    route.fulfill({ json: { kind: "records", table: { id: "acts" }, columns: [], records, total: records.length } }));
}

test("records drawn as bars, grouped, with a milestone and a late dependency in red", async ({ page }) => {
  await mockApi(page);
  await serve(page);
  await loadAs(page, "dept_head");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  const gantt = page.getByTestId("gantt");
  await expect(gantt).toBeVisible({ timeout: 15_000 });
  await expect(gantt.locator("[data-gantt-bar]")).toHaveCount(4);
  // Grouped by project, every label shown whole.
  for (const label of ["Clinic", "School", "Design the clinic", "Build the clinic", "Clinic opens", "Train teachers"]) {
    await expect(gantt.getByText(label, { exact: true })).toBeVisible();
  }
  // The milestone is a diamond (a path), the others bars (rects).
  await expect(gantt.locator('[data-gantt-bar="M1"] path')).toHaveCount(1);
  await expect(gantt.locator('[data-gantt-bar="A1"] rect')).toHaveCount(2); // bar + baseline
  // A2 starts on 1 March, before A1 ends on 31 March: its arrow is red, and says so.
  const late = gantt.locator('path[stroke="var(--color-danger)"]', { has: page.locator("title") });
  await expect(late).toHaveCount(1);
  await expect(late.locator("title")).toHaveText(/Build the clinic follows Design the clinic — but starts 2027-03-01, before it ends 2027-03-31/);
  // Zooming out keeps every bar.
  await page.getByLabel("Timeline zoom").selectOption("year");
  await expect(gantt.locator("[data-gantt-bar]")).toHaveCount(4);
});
