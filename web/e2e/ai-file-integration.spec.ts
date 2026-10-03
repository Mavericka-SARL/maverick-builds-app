/**
 * Data exports and attached-file imports in the developer console:
 * the Integrations tab's Data Export editor (web/src/consoles/developer/
 * ExportSection.tsx) and the AI Developer's spreadsheet chip and
 * "Exports from this session" downloads (AIAssistant.tsx), and a dashboard's
 * Integration button (DashboardWidgets.tsx) sending a workbook as it is to a
 * saved file integration, or downloading an export.
 *
 * Same purpose-built mock as the other specs (no live gateway in the e2e CI
 * job). The server side — validation, rendering from the downloader's own
 * grid view, the AI tools — is covered by
 * internal/gateway/ai_file_integration_test.go against a real database.
 */
import { test, expect, type Page } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("the Data Export editor previews a spec and saves it as a file_export", async ({ page }) => {
  await mockApi(page);
  const previews: Array<Record<string, unknown>> = [];
  const creates: Array<Record<string, unknown>> = [];
  await page.route("**/api/developer/integrations/export-preview", async (route) => {
    previews.push(route.request().postDataJSON());
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({
        header: ["Department", "Region", "opex_marketing"], default_header: ["Department", "Region", "opex_marketing"],
        rows: [["MKT", "EU", "1200.5"]], total_rows: 1, warnings: [], file_name: "OPEX_feed.xlsx", summary: "XLSX · wide",
      }),
    });
  });
  await page.route(url => url.pathname === "/api/developer/integrations", async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    creates.push(route.request().postDataJSON());
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "exp-1", status: "created" }) });
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Integrations" }).first().click();
  await page.getByRole("button", { name: /Data Export/ }).click();
  await page.getByRole("button", { name: "New export" }).click();

  const dialog = page.getByRole("dialog");
  await dialog.getByPlaceholder("e.g. Sales to ERP").fill("OPEX feed");
  await dialog.getByRole("button", { name: "Excel" }).click();
  await expect(page.getByTestId("export-preview")).toContainText("1200.5");
  await expect(dialog.getByText("OPEX_feed.xlsx")).toBeVisible();
  expect(previews.at(-1)).toMatchObject({ target_id: "grid-1", config: { format: "xlsx" } });

  await dialog.getByRole("button", { name: "Create export" }).click();
  await expect.poll(() => creates.length).toBe(1);
  expect(creates[0]).toMatchObject({
    type: "file_export", target_type: "grid", target_id: "grid-1", name: "OPEX feed",
    config: { format: "xlsx", layout: "wide" },
  });
});

test("a spec the server refuses lists every problem and cannot be saved", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/developer/integrations/export-preview", async (route) => {
    await route.fulfill({
      status: 400, contentType: "application/json",
      body: JSON.stringify({
        error: "export spec: …",
        problems: ["a pivot layout needs pivot_dimension — the dimension whose members become columns"],
      }),
    });
  });
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Integrations" }).first().click();
  await page.getByRole("button", { name: /Data Export/ }).click();
  await page.getByRole("button", { name: "New export" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByPlaceholder("e.g. Sales to ERP").fill("Broken");
  await expect(dialog.getByText("a pivot layout needs pivot_dimension")).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Create export" })).toBeDisabled();
});

// ── AI Developer ─────────────────────────────────────────────────────────────

const SESSION_ID = "sess-files";
const session = { id: SESSION_ID, app_id: "app-1", llm_provider: "openai", llm_model: "gpt-4o-mini", created_at: "2026-10-02T09:00:00Z" };

async function mockAiSession(page: Page) {
  await page.route("**/api/ai/sessions", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([session]) });
  });
  await page.route("**/api/ai/settings", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ provider: "openai", model: "gpt-4o-mini", has_key: true }) });
  });
  await page.route(`**/api/ai/sessions/${SESSION_ID}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({
        session, messages: [],
        documents: [{ id: "doc-1", session_id: SESSION_ID, filename: "actuals.xlsx", mime_type: "xlsx", char_count: 60000, truncated: true, importable: true, created_at: session.created_at }],
      }),
    });
  });
  await page.route(`**/api/ai/sessions/${SESSION_ID}/proposals`, async (route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify([{
        id: "prop-1", session_id: SESSION_ID, status: "executed", summary: "Executed 1 step successfully.", created_at: session.created_at,
        steps: [{ tool: "create_export_integration", description: "Create export 'Sales to ERP'", params: { name: "Sales to ERP" }, status: "success", result: "Export 'Sales to ERP' created", created_id: "exp-9" }],
      }]),
    });
  });
}

test("the AI panel marks an importable spreadsheet and downloads the session's exports", async ({ page }) => {
  await mockApi(page);
  await mockAiSession(page);
  await page.route("**/api/integrations/exp-9/export", async (route) => {
    await route.fulfill({
      status: 200, contentType: "text/csv",
      headers: { "Content-Disposition": 'attachment; filename="Sales_to_ERP.csv"' },
      body: "Country;revenue\nCanada;1234,5\n",
    });
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Session/ }).first().click();

  // A whole-kept spreadsheet is not flagged as truncated text.
  const chip = page.getByRole("button", { name: /actuals\.xlsx/ });
  await expect(chip).toHaveAttribute("title", /kept whole/);
  await expect(chip).not.toContainText("⚠");

  const strip = page.getByTestId("session-exports");
  await expect(strip).toContainText("Sales to ERP");
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    strip.getByRole("button", { name: "Download" }).click(),
  ]);
  expect(download.suggestedFilename()).toBe("Sales_to_ERP.csv");
});

// ── Dashboard Integration button ─────────────────────────────────────────────

async function dashboardWithButtons(page: Page) {
  await page.route("**/api/dashboards/dash-5", (route) =>
    route.fulfill({
      json: {
        id: "dash-5", name: "Explainer", tags: ["guide"], folder_id: null,
        widgets: [
          { id: "w-imp", widget_type: "integration_button", ref_id: "int-1", content: "Upload actuals", sort_order: 0,
            pos_x: 0, pos_y: 0, size_w: 240, size_h: 80 },
          { id: "w-exp", widget_type: "integration_button", ref_id: "exp-2", content: "Download for ERP", sort_order: 1,
            pos_x: 260, pos_y: 0, size_w: 240, size_h: 80 },
        ],
      },
    }));
  await page.route(url => url.pathname === "/api/integrations", (route) =>
    route.fulfill({
      json: [
        { id: "int-1", name: "Monthly actuals", type: "csv_import", target_type: "grid", target_id: "grid-1", config: {} },
        { id: "exp-2", name: "ERP feed", type: "file_export", target_type: "grid", target_id: "grid-1", config: { format: "csv" } },
      ],
    }));
}

test("a dashboard Integration button sends a workbook as it is and names the first bad row", async ({ page }) => {
  await mockApi(page);
  await dashboardWithButtons(page);
  const bodies: Array<Record<string, unknown>> = [];
  await page.route("**/api/integrations/int-1/run", async (route) => {
    bodies.push(route.request().postDataJSON());
    await route.fulfill({ json: { rows_imported: 2, values_imported: 4, error_rows: 1,
      errors: [{ row: 3, column: "Country", code: "UNKNOWN_MEMBER", message: "\"FR\" is not a member of dimension \"geography\"" }] } });
  });
  await loadAs(page, "business_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "Explainer" }).first().click();

  const [chooser] = await Promise.all([
    page.waitForEvent("filechooser"),
    page.getByRole("button", { name: "Upload actuals" }).click(),
  ]);
  // Any bytes do: the button does not parse the workbook, the server does.
  await chooser.setFiles({ name: "actuals.xlsx", mimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buffer: Buffer.from("PK\u0003\u0004 workbook") });
  await expect.poll(() => bodies.length).toBe(1);
  expect(bodies[0]).toEqual({ xlsx_base64: Buffer.from("PK\u0003\u0004 workbook").toString("base64") });
  await expect(page.getByText("2 rows imported, 1 errors")).toBeVisible();
  await expect(page.getByText(/Row 3: "FR" is not a member/)).toBeVisible();

  // A CSV still goes as text.
  const [chooser2] = await Promise.all([
    page.waitForEvent("filechooser"),
    page.getByRole("button", { name: /Done|Upload actuals/ }).first().click(),
  ]);
  await chooser2.setFiles({ name: "actuals.csv", mimeType: "text/csv", buffer: Buffer.from("Country,Amount\nCA,1\n") });
  await expect.poll(() => bodies.length).toBe(2);
  expect(bodies[1]).toEqual({ csv: "Country,Amount\nCA,1\n" });
});

test("a dashboard Integration button on an export downloads the file", async ({ page }) => {
  await mockApi(page);
  await dashboardWithButtons(page);
  await page.route("**/api/integrations/exp-2/export", (route) =>
    route.fulfill({ status: 200, contentType: "text/csv", headers: { "Content-Disposition": 'attachment; filename="ERP_feed.csv"' }, body: "a;b\n1;2\n" }));
  await loadAs(page, "business_admin");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Dashboards" }).click();
  await page.getByRole("button", { name: "Explainer" }).first().click();
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("button", { name: "Download for ERP" }).click(),
  ]);
  expect(download.suggestedFilename()).toBe("ERP_feed.csv");
});

// ── Reshaped files ───────────────────────────────────────────────────────────

test("the AI panel downloads a converted file as CSV and as Excel", async ({ page }) => {
  await mockApi(page);
  await mockAiSession(page);
  await page.route(url => url.pathname === `/api/ai/sessions/${SESSION_ID}/conversions`, (route) =>
    route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify([{ id: "conv-1", session_id: SESSION_ID, document_id: "doc-1", filename: "plan (converted)", row_count: 5, columns: ["geography", "metric_id", "period", "value"], created_at: session.created_at }]),
    }));
  const formats: string[] = [];
  await page.route(url => url.pathname === `/api/ai/sessions/${SESSION_ID}/conversions/conv-1`, async (route) => {
    const format = new URL(route.request().url()).searchParams.get("format") ?? "";
    formats.push(format);
    await route.fulfill({
      status: 200, contentType: format === "xlsx" ? "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" : "text/csv",
      headers: { "Content-Disposition": `attachment; filename="plan_converted.${format}"` },
      body: "geography,metric_id,period,value\nCA,Revenue,Q1,10000\n",
    });
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: /Session/ }).first().click();

  const strip = page.getByTestId("session-conversions");
  await expect(strip).toContainText("plan (converted)");
  await expect(strip).toContainText("5 rows");
  for (const [label, file] of [["CSV", "plan_converted.csv"], ["Excel", "plan_converted.xlsx"]] as const) {
    const [download] = await Promise.all([
      page.waitForEvent("download"),
      strip.getByRole("button", { name: `Download plan (converted) as ${label}` }).click(),
    ]);
    expect(download.suggestedFilename()).toBe(file);
  }
  expect(formats).toEqual(["csv", "xlsx"]);
});

test("a file integration the AI set up to reshape says how in the Integrations tab", async ({ page }) => {
  await mockApi(page);
  await page.route(url => url.pathname === "/api/developer/integrations" || url.pathname === "/api/integrations", (route) =>
    route.request().method() !== "GET" ? route.fallback() : route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify([{
        id: "int-r", name: "Plan upload", type: "csv_import", target_type: "grid", target_id: "grid-1", status: "active",
        config: {
          column_map: { Country: "geography", Quarter: "period", Line: "metric" },
          reshape: { header_row: 3, fill_down: ["Country"], skip_rows: [{ column: "Country", equals: "Total" }],
            unpivot: { from: "Quarter 1", to: "Quarter 2", name_column: "Quarter", value_column: "value" }, scale: { value: 1000 } },
        },
      }]),
    }));
  await loadAs(page, "developer");
  await page.getByRole("button", { name: "Integrations" }).first().click();
  await expect(page.getByTestId("integration-reshape-int-r")).toHaveText(
    "Reshapes each file: header on row 3 · fills down Country · skips 1 kind of row · turns Quarter 1…Quarter 2 into rows (Quarter, value) · value ×1000");

  // The wizard cannot show a reshape, so the run sends the file as it is
  // and the server reshapes it — never the wizard's own mapped CSV.
  await expect(page.getByRole("button", { name: "Run Import" })).toHaveCount(0);
  let sent: Record<string, unknown> | null = null;
  await page.route("**/api/integrations/int-r/run", async (route) => {
    sent = route.request().postDataJSON();
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ rows_imported: 2, values_imported: 4, error_rows: 0, errors: [] }) });
  });
  await page.getByLabel("File for Plan upload").setInputFiles({ name: "plan.csv", mimeType: "text/csv", buffer: Buffer.from("Plan FY26\n\nCountry,Line,Quarter 1\nCanada,Revenue,10\n") });
  await expect(page.getByRole("status").filter({ hasText: "4 value(s) imported" })).toBeVisible();
  expect(sent).toEqual({ csv: "Plan FY26\n\nCountry,Line,Quarter 1\nCanada,Revenue,10\n" });
});
