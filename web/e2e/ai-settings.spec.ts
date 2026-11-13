/**
 * AI Developer Settings (web/src/consoles/developer/AIAssistant.tsx): the model
 * is free text, offered the models the key reaches (asked from the provider,
 * so new models appear as they ship), Google is offered, a model name never
 * survives a change of provider (blank then means the provider's default),
 * and an OpenAI key's reasoning effort is saved with the settings.
 */
import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

test("the model is free text offered the key's models, Google is offered, and switching provider clears the model", async ({ page }) => {
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

  await page.route("**/api/ai/settings/models", async (route) => {
    const body = route.request().postDataJSON() as { provider?: string };
    const models = body.provider === "anthropic" ? ["claude-opus-4-8", "claude-sonnet-4-8"] : ["gemini-3-pro"];
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ provider: body.provider, models }) });
  });

  await loadAs(page, "developer");
  await page.getByRole("button", { name: "AI Developer" }).click();
  await page.getByRole("button", { name: "AI settings" }).click();

  const model = page.getByLabel("Model", { exact: true });
  await expect(model).toHaveValue("claude-opus-4-8");
  await expect(page.locator("#ai-model-options option")).toHaveCount(2);
  await expect(page.getByLabel("Reasoning effort")).toHaveCount(0);

  const provider = page.getByLabel("LLM provider");
  await expect(provider.locator("option")).toHaveText(["OpenAI", "Anthropic (Claude)", "Google (Gemini)", "Mistral", "DeepSeek"]);
  await provider.selectOption("google");
  await expect(model).toHaveValue("");
  await expect(model).toHaveAttribute("placeholder", "Leave blank for the provider's default");

  await model.fill("gemini-99-ultra-tomorrow");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect.poll(() => saved).toEqual({ provider: "google", model: "gemini-99-ultra-tomorrow", reasoning_effort: "" });

  await provider.selectOption("openai");
  await page.getByLabel("Reasoning effort").selectOption("none");
  await model.fill("gpt-6-luna");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect.poll(() => saved).toEqual({ provider: "openai", model: "gpt-6-luna", reasoning_effort: "none" });
  // One model runs every turn: no second model field (removed 2026-11-12).
  await expect(page.getByLabel("Model for generated sheet plans")).toHaveCount(0);
});
