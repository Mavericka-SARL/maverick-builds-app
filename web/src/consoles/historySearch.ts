import type { HistoryInstance } from "../api/client";

// The Workflow History's search (owner, 2026-11-18: "search by submitter,
// status, stage, model etc."), as the Inbox's (business/taskSearch.ts): a
// run matches when every word typed is somewhere in what its card shows —
// the workflow, who submitted it, the model, its status, the stage it waits
// at, every step with its decision and comment, the scope it is about and
// its dates. Component-free, as react-refresh asks of a file the two
// histories share.

/** A history page's size (GET /api/workflow/history: historyPageSize). */
export const HISTORY_PAGE_SIZE = 50;

/** The steps a run waits at now: its in-progress steps' names. */
export function currentStages(inst: HistoryInstance): string[] {
  return (inst.steps ?? []).filter((s) => s.status === "in_progress").map((s) => s.step_name || s.step_def_id);
}

/** Everything a run's card shows, lower-cased, words apart. */
export function historySearchText(inst: HistoryInstance, dateText: (iso: string) => string): string {
  const parts: (string | undefined)[] = [
    inst.workflow_name, inst.status, inst.submitted_by, inst.model_name, inst.context?.revision,
    dateText(inst.started_at), inst.completed_at ? dateText(inst.completed_at) : undefined,
  ];
  const stages = currentStages(inst);
  if (stages.length) parts.push("stage", "waiting at", ...stages);
  for (const s of inst.steps ?? []) parts.push(s.step_name, s.step_def_id, s.step_type, s.status, s.decision, s.comment);
  for (const c of inst.context_display ?? []) parts.push(c.dimension_name, c.key, c.display, c.value);
  for (const [k, v] of Object.entries(inst.context ?? {})) parts.push(k, String(v));
  return parts.filter(Boolean).join(" · ").toLowerCase();
}

/** The distinct non-empty values, sorted, for a filter's options. */
export function distinctValues(values: (string | undefined)[]): string[] {
  return [...new Set(values.filter((v): v is string => !!v))].sort((a, b) => a.localeCompare(b));
}
