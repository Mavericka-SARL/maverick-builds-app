import type { PlanLimits } from "../api/client";

/**
 * "5 users · 2 applications · 3 models" from a plan's limits, or "" when the
 * plan limits nothing that is worth saying out loud.
 *
 * Shared by /signup and the terms of service on purpose: both describe the
 * same offer to the same visitor, and two copies of this formatting would
 * eventually disagree about it. Zero means unlimited everywhere in
 * internal/plan, so a zero is simply left out.
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

/** The same limits inside a sentence: "5 users, 2 applications and 3 models". */
export function planAllowanceProse(limits: PlanLimits | undefined): string {
  const parts = planAllowanceParts(limits);
  if (parts.length < 2) return parts.join("");
  return `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}`;
}
