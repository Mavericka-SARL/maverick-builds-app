import type { PlanLimits } from "../api/client";

/**
 * "5 users · 2 applications · 3 models" from a plan's limits, or "" when the
 * plan limits nothing that is worth saying out loud.
 *
 * The terms of service quote the same limits in a sentence, written by the
 * gateway (planAllowanceProse in internal/gateway/legal_documents.go) with
 * the same rules. Zero means unlimited everywhere in internal/plan, so a zero
 * is simply left out.
 */
export function planAllowanceParts(limits: PlanLimits | undefined): string[] {
  if (!limits) return [];
  const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;
  return [
    limits.max_users ? plural(limits.max_users, "user") : "",
    limits.max_applications ? plural(limits.max_applications, "application") : "",
    limits.max_models ? plural(limits.max_models, "model") : "",
    limits.max_storage_mb ? `${limits.max_storage_mb} MB of data` : "",
  ].filter(Boolean);
}

/** The strip under /signup's heading: "5 users · 2 applications · 3 models". */
export function planAllowance(limits: PlanLimits | undefined): string {
  return planAllowanceParts(limits).join(" · ");
}
