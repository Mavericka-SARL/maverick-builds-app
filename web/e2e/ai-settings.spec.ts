/**
 * AI Developer Settings (web/src/consoles/developer/AIAssistant.tsx): the model
 * is free text with no suggestion list — providers ship new models faster
 * than a list could follow — Google is offered, and a model name never
 * survives a change of provider (blank then means the provider's default).
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("the model is free text, Google is offered, and switching provider clears the model", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/ai/sessions", (route) =>
    route.request().method() === "GET" ? route.fulfill({ status: 200, contentType: "application/json", body: "[]" }) : route.fallback());
  let saved: Record<string, unknown> | null = null;
  await page.route("**/api/ai/settings", async (route) => {
    if (route.request().method() === "PUT") {
      saved = route.request().postDataJSON() as Record<string, unknown>;
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok" }) });
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ provider: "anthropic", model: "claude-opus-4-8", has_key: true }) });
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: "AI settings" }).click();

  const model = page.getByLabel("Model", { exact: true });
  await expect(model).toHaveValue("claude-opus-4-8");
  await expect(model).not.toHaveAttribute("list", /.*/);
  await expect(page.locator("datalist")).toHaveCount(0);

  const provider = page.getByLabel("LLM provider");
  await expect(provider.locator("option")).toHaveText(["OpenAI", "Anthropic (Claude)", "Google (Gemini)", "Mistral", "DeepSeek"]);
  await provider.selectOption("google");
  await expect(model).toHaveValue("");
  await expect(model).toHaveAttribute("placeholder", "Leave blank for the provider's default");

  await model.fill("gemini-99-ultra-tomorrow");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect.poll(() => saved).toEqual({ provider: "google", model: "gemini-99-ultra-tomorrow" });
});
