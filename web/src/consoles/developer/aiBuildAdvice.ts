/**
 * Whether a model and reasoning effort suit building a model from a workbook —
 * the same rule as the gateway's buildAdvice (ai_task.go), so the note follows
 * the settings as they are changed, before saving. "" when nothing is to be said.
 */
export function buildAdvice(provider: string, model: string, effort: string): string {
  if ((provider || "openai") !== "openai") return "";
  const m = model.trim().toLowerCase();
  if (effort === "none" || effort === "minimal") {
    return `Reasoning effort ${effort} plans too shallowly to build a model from a workbook: fine for questions, pages and small edits. For builds choose medium or high.`;
  }
  if (m.includes("mini") || m.includes("nano")) {
    return "A small model: good for pages, wiring and small edits with an exact request. To build a model from a workbook, use a reasoning model (gpt-6.1-sol, gpt-5.5) at medium or high effort.";
  }
  return "";
}
