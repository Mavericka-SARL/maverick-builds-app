/**
 * The business Import widget's template is written by the gateway (POST
 * /api/import/template-workbook) from the grid the user sees: its dimension
 * names and writable metrics, and an example row. The browser used to write
 * it with SheetJS.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("the Import widget downloads a template the gateway wrote", async ({ page }) => {
  await mockApi(page);
  const dashboard = {
    id: "dash-import", name: "Upload actuals", tags: [], folder_id: null,
    widgets: [{ id: "wi", widget_type: "import", ref_id: "grid-1", content: null, sort_order: 0, col_start: 1, col_span: 12,
      pos_x: 0, pos_y: 0, size_w: 800, size_h: 500 }],
  };
  await page.route((url) => url.pathname === "/api/dashboards", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([dashboard]) }));
  await page.route((url) => url.pathname.startsWith("/api/dashboards/"), (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(dashboard) }));
  let asked: { rows?: unknown[][] } | undefined;
  await page.route("**/api/import/template-workbook", async (route) => {
    asked = route.request().postDataJSON();
    await route.fulfill({ status: 200, body: "PK workbook",
      headers: { "Content-Type": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        "Content-Disposition": 'attachment; filename="import-template.xlsx"' } });
  });
  await loadAs(page, "dept_head");
  await expect(page.getByRole("heading", { name: "Upload actuals" })).toBeVisible();
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download .xlsx template" }).click();
  expect((await download).suggestedFilename()).toBe("import-template.xlsx");
  expect(asked?.rows?.length).toBe(2);
  expect(Array.isArray(asked?.rows?.[0]) && (asked!.rows![0] as unknown[]).length).toBeGreaterThan(0);
});
