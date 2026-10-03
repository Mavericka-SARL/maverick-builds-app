/**
 * The Import Wizard's Shape step (ShapeStep.tsx): a developer reshapes a
 * file laid out for people — a title above the header, months across the
 * columns — previews it by the server's own code, maps the shaped columns
 * and saves the reshape with the integration; and later edits a saved
 * integration's shape and mapping without importing. Before this the only
 * way to set a reshape up was the AI Developer.
 *
 * The server side (POST /api/import/reshape-preview, every run applying a
 * saved reshape) is covered by internal/gateway/ai_file_reshape_test.go.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

const json = (body: unknown, status = 200) => ({ status, contentType: "application/json", body: JSON.stringify(body) });
const FILE = "Plan FY26\n\nCountry,Jan,Feb\nCanada,10,12\n";

async function routes(page: Page, integrations: unknown[] = []) {
  const previews: Array<Record<string, unknown>> = [];
  const configs: Array<{ url: string; body: Record<string, unknown> }> = [];
  await page.route("**/api/import/reshape-preview", async (route) => {
    const body = route.request().postDataJSON();
    previews.push(body);
    await route.fulfill(json({
      raw: [["Plan FY26"], [], ["Country", "Jan", "Feb"], ["Canada", "10", "12"]],
      header: ["Department", "Region", "metric", "value"],
      rows: [["SALES", "EMEA", "opex_people", "10"], ["SALES", "EMEA", "opex_marketing", "12"]],
      row_count: 2,
    }));
  });
  await page.route("**/api/developer/revisions*", (route) => route.fulfill(json([{ id: "rev-1", name: "Working", is_active: true }])));
  await page.route(url => url.pathname === "/api/developer/integrations" || url.pathname === "/api/integrations", (route) =>
    route.request().method() === "POST" ? route.fulfill(json({ id: "int-new" })) : route.fulfill(json(integrations)));
  await page.route(url => /\/api\/developer\/integrations\/[^/]+\/config$/.test(url.pathname), async (route) => {
    configs.push({ url: route.request().url(), body: route.request().postDataJSON().config });
    await route.fulfill(json({ status: "updated" }));
  });
  return { previews, configs };
}

async function upload(page: Page) {
  await page.getByLabel("Grid", { exact: true }).selectOption({ label: "OPEX Planning Grid" });
  await page.getByLabel("Target revision").selectOption("rev-1");
  await page.locator('input[type="file"]').setInputFiles({ name: "plan.csv", mimeType: "text/csv", buffer: Buffer.from(FILE) });
  await page.getByRole("button", { name: "Next: Shape" }).click();
}

test("a developer shapes a file, previews it and saves the shape with the integration", async ({ page }) => {
  await mockApi(page);
  const { previews, configs } = await routes(page);
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "New Import" }).click();
  await upload(page);

  const shape = page.getByTestId("shape-step");
  await shape.getByLabel("Header row").fill("3");
  await shape.getByLabel("Turn columns into rows").check();
  await shape.getByLabel("First column to turn").fill("Jan");
  await shape.getByLabel("Last column to turn").fill("Feb");
  await shape.getByLabel("Column for the headers").fill("Month");
  await shape.getByLabel("Column for the values").fill("value");
  await shape.getByRole("button", { name: "Preview" }).click();
  await expect(page.getByTestId("shape-preview")).toContainText("Region");
  await expect(page.getByRole("status")).toHaveText("2 row(s) after shaping");
  expect(previews[0]).toMatchObject({ csv: FILE, reshape: { header_row: 3, unpivot: { from: "Jan", to: "Feb", name_column: "Month", value_column: "value" } } });

  await page.getByRole("button", { name: "Next: Map Columns", exact: true }).click();
  // The map step reads the SHAPED columns.
  for (const col of ["Department", "Region", "metric"]) await expect(page.getByRole("cell", { name: col, exact: true }).first()).toBeVisible();
  await page.getByRole("button", { name: "Save as Integration" }).click();
  await page.getByPlaceholder("Integration name (e.g. Import OPEX Data)").fill("Plan upload");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => configs.length).toBe(1);
  expect(configs[0].body.reshape).toEqual({ header_row: 3, unpivot: { from: "Jan", to: "Feb", name_column: "Month", value_column: "value" } });
  expect(Object.keys(configs[0].body.column_map as object)).toEqual(expect.arrayContaining(["Department", "Region", "metric", "value"]));
});

test("a developer edits a saved integration's shape and mapping without importing", async ({ page }) => {
  await mockApi(page);
  const saved = {
    id: "int-r", name: "Plan upload", type: "csv_import", target_type: "grid", target_id: "grid-1", status: "active",
    config: { column_map: { Department: "Department", Region: "Region", metric: "metric", value: "value" }, import_mode: "replace", reshape: { header_row: 3, unpivot: { from: "Jan", to: "Feb", name_column: "Month", value_column: "value" } } },
  };
  const { previews, configs } = await routes(page, [saved]);
  await loadAs(page, "developer");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Integrations" }).click();
  await page.getByRole("button", { name: "Edit shape and mapping of Plan upload" }).click();
  await upload(page);

  // The saved shape is loaded; one more step is added to it.
  const shape = page.getByTestId("shape-step");
  await expect(shape.getByLabel("Header row")).toHaveValue("3");
  await expect(shape.getByLabel("First column to turn")).toHaveValue("Jan");
  await shape.getByRole("button", { name: "Add a fixed value" }).click();
  await shape.getByLabel("Fixed value 1 column").fill("Scenario");
  await shape.getByLabel("Fixed value 1 value").fill("Budget");
  await page.getByRole("button", { name: "Next: Map Columns", exact: true }).click();
  expect(previews.at(-1)?.reshape).toMatchObject({ header_row: 3, constants: { Scenario: "Budget" } });

  await page.getByRole("button", { name: "Save changes" }).click();
  await expect.poll(() => configs.length).toBe(1);
  expect(configs[0].url).toContain("/api/developer/integrations/int-r/config");
  expect(configs[0].body).toMatchObject({
    import_mode: "replace", // kept
    reshape: { header_row: 3, constants: { Scenario: "Budget" }, unpivot: { from: "Jan", to: "Feb" } },
  });
  expect(configs[0].body.column_map).toEqual({ Department: "Department", Region: "Region", metric: "metric", value: "value" }); // the saved mapping carried over
});
