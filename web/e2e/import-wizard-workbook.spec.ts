/**
 * The Import Wizard reads a chosen workbook through the gateway (POST
 * /api/import/parse-workbook): the browser used to parse it with SheetJS,
 * which no scanner covers. The sheet picker reads the same way.
 *
 * The server side is internal/gateway/import_workbooks_test.go.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

test("a chosen workbook is read by the gateway, sheet by sheet", async ({ page }) => {
  await mockApi(page);
  const asked: Array<{ xlsx_base64?: string; sheet?: string }> = [];
  await page.route("**/api/import/parse-workbook", async (route) => {
    const body = route.request().postDataJSON();
    asked.push(body);
    const data = body.sheet === "Data";
    await route.fulfill(json({
      sheets: ["Notes", "Data"], sheet: data ? "Data" : "Notes",
      headers: data ? ["Country", "Revenue"] : ["Remark"],
      rows: data ? [["Canada", "1234.5"]] : [["see Data"]],
    }));
  });
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "New Import" }).click();
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "OPEX Planning Grid" });
  await page.getByLabel("Target revision").selectOption("rev-1");
  const workbook = Buffer.from("PK\u0003\u0004 a workbook");
  await page.locator('input[type="file"]').setInputFiles({ name: "plan.xlsx",
    mimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buffer: workbook });

  await expect(page.getByRole("columnheader", { name: "Remark" })).toBeVisible();
  expect(asked[0]?.xlsx_base64).toBe(workbook.toString("base64"));
  await page.getByLabel("Sheet").selectOption("Data");
  await expect(page.getByRole("columnheader", { name: "Revenue" })).toBeVisible();
  await expect(page.getByText("1234.5")).toBeVisible();
  expect(asked.at(-1)?.sheet).toBe("Data");
});
