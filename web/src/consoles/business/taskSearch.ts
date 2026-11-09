import type { Task } from "../../api/client";

// The Workflow Inbox's search (owner, 2026-11-09: "search by any parameter —
// workflow name, model, status, steps"): a task matches when every word typed
// is somewhere in what its card shows — the workflow, step and model names,
// the step's type and state (overdue, due soon, sent back), who asked, the
// instructions, the scope it is about and its dates. Component-free, as
// react-refresh asks of a file the two Inboxes share.

const DAY_MS = 24 * 60 * 60 * 1000;

/** Everything a task's card shows, lower-cased, words apart. */
export function taskSearchText(t: Task, dateText: (iso: string) => string, now: Date = new Date()): string {
  const parts: (string | undefined)[] = [
    t.workflow_name, t.step_name, t.step_def_id, t.step_type, t.model_name, t.requested_by,
    t.instructions, t.context?.revision, "pending", dateText(t.created_at),
  ];
  if (t.rework_count) parts.push("sent back", "rework", t.rework_note);
  if (t.due_at) {
    const due = new Date(t.due_at).getTime();
    parts.push("due", dateText(t.due_at));
    if (due < now.getTime()) parts.push("overdue");
    else if (due - now.getTime() < DAY_MS) parts.push("due soon");
  }
  for (const c of t.context_display ?? []) parts.push(c.dimension_name, c.key, c.display, c.value);
  for (const [k, v] of Object.entries(t.context ?? {})) parts.push(k, String(v));
  return parts.filter(Boolean).join(" · ").toLowerCase();
}

/** Whether every word of query is in text (taskSearchText). */
export function matchesAllWords(text: string, query: string): boolean {
  return query.toLowerCase().split(/\s+/).filter(Boolean).every((w) => text.includes(w));
}

/** The search box's placeholder in both Inboxes. */
export const TASK_SEARCH_PLACEHOLDER = "Search by workflow, model, step, status…";
