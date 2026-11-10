/**
 * The Workflow Inbox lists every application's tasks; opening a task of
 * another model switches the consoles to that model, and opening one of the
 * model already selected changes nothing (owner decision 2026-11-09).
 * Mocked: CI runs without a gateway.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test.use({ actionTimeout: 10_000 });

const base = { context: {}, status: "in_progress", created_at: "2026-11-09T08:00:00Z", required_comment: false, completion_label: "Complete", instructions: "", step_type: "task", workflow_name: "Close" };
const tasks = [
  { ...base, id: "t-here", instance_id: "i-1", step_def_id: "s1", step_name: "Task here", application_id: "app-1", model_id: "model-1", model_name: "Finance Model" },
  { ...base, id: "t-other", instance_id: "i-2", step_def_id: "s2", step_name: "Task of the forecast", application_id: "app-1", model_id: "model-2", model_name: "Forecast Model" },
  { ...base, id: "t-app", instance_id: "i-3", step_def_id: "s3", step_name: "Task of sales", application_id: "app-2", model_id: "model-3", model_name: "Sales Model" },
];
const apps = [
  { id: "app-1", name: "Planning", mode: "planning", models: [{ id: "model-1", name: "Finance Model", is_default: true }, { id: "model-2", name: "Forecast Model" }] },
  { id: "app-2", name: "Sales", mode: "planning", models: [{ id: "model-3", name: "Sales Model", is_default: true }] },
];

test("inbox: opening a task of another model switches to it", async ({ page }) => {
  await mockApi(page);
  const json = (body: unknown) => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/tasks", (route) => route.fulfill(json(tasks)));
  await page.route("**/api/workflow/definitions", (route) => route.fulfill(json([])));
  await page.route((url) => url.pathname === "/api/apps", (route) => route.fulfill(json(apps)));

  await loadAs(page, "dept_head");
  await page.getByRole("button", { name: "Workflow Inbox", exact: true }).click();
  await expect(page.getByText("Task here")).toBeVisible({ timeout: 15_000 });
  // A switch shows in what the console asks next: the mocked context always
  // answers app-1, so the selection it stores is not read back here.
  const asksFor = (app: string, model?: string) => page.waitForRequest((req) =>
    req.url().includes("/api/") && req.headers()["x-app-id"] === app && (model === undefined || req.headers()["x-model-id"] === model));

  // The selected application's default model: nothing switches.
  await page.getByText("Task here", { exact: true }).click();
  expect(await page.evaluate(() => localStorage.getItem("selected_model_id"))).toBeNull();

  const toForecast = asksFor("app-1", "model-2");
  await page.getByText("Task of the forecast", { exact: true }).click();
  await toForecast;

  await expect(page.getByText("Task of sales", { exact: true })).toBeVisible();
  const toSales = asksFor("app-2");
  await page.getByText("Task of sales", { exact: true }).click();
  await toSales;
});
