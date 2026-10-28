/**
 * The Import Wizard reads a chosen workbook through the gateway (POST
 * /api/import/parse-workbook): the browser used to parse it with SheetJS,
 * which no scanner covers. The sheet picker reads the same way.
 *
 * The server side is internal/gateway/import_workbooks_test.go.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs, acceptLiveEdits } from "./mocks";

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

// A loader saved from a workbook keeps the sheet it was built from, so a run
// whose file names none (a dashboard's Integration button) reads that sheet,
// not the first one; Edit opens the next workbook on it (2026-10-28). The
// server side is TestAFileLoaderReadsTheSheetItWasSavedWith.
test("a loader saved from a workbook keeps its sheet, and Edit opens on it", async ({ page }) => {
  await mockApi(page);
  const asked: Array<{ sheet?: string }> = [];
  await page.route("**/api/import/parse-workbook", async (route) => {
    const body = route.request().postDataJSON();
    asked.push(body);
    const data = body.sheet === "Data";
    await route.fulfill(json({
      sheets: ["Notes", "Data"], sheet: data ? "Data" : "Notes",
      headers: data ? ["Department", "Region", "metric", "value"] : ["Remark"],
      rows: data ? [["SALES", "EMEA", "opex_people", "10"]] : [["see Data"]],
    }));
  });
  const saved = {
    id: "int-w", name: "Actuals upload", type: "csv_import", target_type: "grid", target_id: "grid-1", status: "active",
    config: { column_map: { Department: "Department", Region: "Region", metric: "metric", value: "value" }, import_mode: "replace", sheet: "Data" },
  };
  const configs: Array<Record<string, unknown>> = [];
  const shaped: Array<{ sheet?: string }> = [];
  await page.route("**/api/import/reshape-preview", async (route) => {
    shaped.push(route.request().postDataJSON());
    await route.fulfill(json({ raw: [["Department", "Region", "metric", "value"]], header: ["Department", "Region", "metric", "value"],
      rows: [["SALES", "EMEA", "opex_people", "10"]], row_count: 1 }));
  });
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
  await acceptLiveEdits(page, "rev-1");
  await page.route(url => url.pathname === "/api/developer/integrations" || url.pathname === "/api/integrations", (route) =>
    route.request().method() === "POST" ? route.fulfill(json({ id: "int-new" })) : route.fulfill(json([saved])));
  await page.route(url => /\/api\/developer\/integrations\/[^/]+\/config$/.test(url.pathname), async (route) => {
    configs.push(route.request().postDataJSON().config);
    await route.fulfill(json({ status: "updated" }));
  });
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await expect(page.getByTestId("integration-sheet-int-w")).toHaveText("Reads sheet Data");

  const workbook = { name: "actuals.xlsx", mimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buffer: Buffer.from("PK\u0003\u0004 a workbook") };
  await page.getByRole("button", { name: "New Import" }).click();
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "OPEX Planning Grid" });
  await page.getByLabel("Target revision").selectOption("rev-1");
  await page.locator('input[type="file"]').setInputFiles(workbook);
  await page.getByLabel("Sheet").selectOption("Data");
  await expect(page.getByRole("columnheader", { name: "Region" })).toBeVisible();
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: "Next: Map Columns (no shaping)" }).click();
  await page.getByRole("button", { name: "Save as Integration" }).click();
  await page.getByPlaceholder("Integration name (e.g. Import OPEX Data)").fill("Actuals upload 2");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => configs.length).toBe(1);
  expect(configs[0].sheet).toBe("Data");

  // Edit opens a workbook on the loader's saved sheet, and saving keeps it.
  await page.reload();
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "Edit shape and mapping of Actuals upload" }).click();
  asked.length = 0;
  await page.locator('input[type="file"]').setInputFiles(workbook);
  await expect(page.getByLabel("Sheet")).toHaveValue("Data");
  expect(asked[0]?.sheet).toBe("Data");
  await page.getByRole("button", { name: "Next: Shape" }).click();
  await page.getByRole("button", { name: "Next: Map Columns (no shaping)" }).click();
  expect(shaped.at(-1)?.sheet).toBe("Data");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect.poll(() => configs.length).toBe(2);
  expect(configs[1].sheet).toBe("Data");
});
