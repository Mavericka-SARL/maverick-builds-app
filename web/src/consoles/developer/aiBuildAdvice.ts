/**
 * Whether a model and reasoning effort suit building a model from a workbook —
 * the same rule as the gateway's buildAdvice (ai_task.go), so the note follows
 * the settings as they are changed, before saving. "" when nothing is to be said.
 */
export function buildAdvice(provider: string, model: string, effort: string): string {
  if ((provider || "openai") !== "openai") return "";
  const m = model.trim().toLowerCase();
  if (effort === "none" || effort === "minimal") {
    return `Reasoning effort ${effort} plans too shallowly to build a model from a workbook: fine for questions, pages and small edits. For builds choose low or higher.`;
  }
  if (m.includes("mini") || m.includes("nano")) {
    return "A small model: good for pages, wiring and small edits with an exact request. To build a model from a workbook, use a strong reasoning model (gpt-6.1-sol, gpt-5.5) at low effort: " +
      "it finishes in fewer calls and less time, where a small model asks more and stops short.";
  }
  if (effort === "high" || effort === "xhigh") {
    return `Low effort is usually enough to build a model from a workbook with this model; ${effort} makes every call longer and costlier. Raise it only when plans come back too shallow.`;
  }
  return "";
}
