import { useQuery } from "@tanstack/react-query";
import type { DesignTone } from "../../ui";
import { api } from "../../api/client";
import type { StepType, TriggerEventCatalogItem } from "../../api/client";

// The workflow's trigger event (a catalog key) → the rule trigger type the
// engine dispatches on. Per-form keys ("expense_request.submitted") and
// per-integration keys ("actuals_csv.import.completed") used to fall
// through to "manual", so a rule created from such a workflow never fired
// on the event it was designed for. Build › Triggers and the workflow's
// Create Trigger dialog both use this one mapping.
export function triggerTypeFromWorkflow(triggerEvent: string): string {
  if (triggerEvent === "form.submit" || triggerEvent.endsWith(".submitted")) return "form_submit";
  if (triggerEvent === "api.workflow.start") return "api";
  if (triggerEvent.endsWith(".import.completed")) return "integration_completed";
  if (triggerEvent.endsWith(".import.failed")) return "integration_failed";
  return "manual";
}

// The events that name no source: a rule made from them fires for any form
// or any integration run, which is what they say.
const ANY_SOURCE_EVENTS = new Set(["form.submit", "integration.import.completed", "integration.import.failed"]);

export interface RuleTriggerFromWorkflow {
  trigger_type: string;
  source_form_id: string;
  source_integration_id: string;
  /** A per-form or per-integration event whose form or integration the
      catalog does not list (still loading, renamed, or not in this
      revision): a rule made now would fire for every source. */
  unresolved: boolean;
}

/** The rule a workflow's trigger event asks for: the trigger type, and the
    one form or integration a per-source event ("expense_request.submitted",
    "actuals_csv.import.completed") names, looked up in the trigger-event
    catalog of the revision the rule is made in. The dispatcher reads an
    empty source as "any", so a rule without it started the workflow on
    every form submitted or every import in the revision. */
export function ruleTriggerFromWorkflow(triggerEvent: string, catalog: TriggerEventCatalogItem[] | undefined): RuleTriggerFromWorkflow {
  const trigger_type = triggerTypeFromWorkflow(triggerEvent);
  const out = { trigger_type, source_form_id: "", source_integration_id: "", unresolved: false };
  if (trigger_type === "manual" || trigger_type === "api" || ANY_SOURCE_EVENTS.has(triggerEvent)) return out;
  const item = catalog?.find(c => c.key === triggerEvent);
  if (item?.source_type === "form" && item.source_id) return { ...out, source_form_id: item.source_id };
  if (item?.source_type === "integration" && item.source_id) return { ...out, source_integration_id: item.source_id };
  return { ...out, unresolved: true };
}

export const STEP_TYPE_LABELS: Record<StepType, string> = {
  task: "Task",
  approval: "Approval",
  condition: "Condition",
  notification: "Notification",
  join: "Join",
};

export const STATUS_TONES: Record<string, { tone: DesignTone; label: string }> = {
  draft:     { tone: "draft",   label: "Draft" },
  published: { tone: "live",    label: "Published" },
  archived:  { tone: "warning", label: "Archived" },
  invalid:   { tone: "danger",  label: "Invalid" },
};

export const TRIGGER_FALLBACK: TriggerEventCatalogItem[] = [
  {
    key: "manual", label: "Manual", description: "Started manually.",
    category: "manual", source_type: "system", payload_schema: [], enabled: true, created_from: "system",
  },
  {
    key: "api.workflow.start", label: "API trigger", description: "Started via API.",
    category: "api", source_type: "system", payload_schema: [], enabled: true, created_from: "system",
  },
];

export const TRIGGER_CATEGORY_ORDER: TriggerEventCatalogItem["category"][] = [
  "manual", "form", "integration", "planning", "api",
];
export const TRIGGER_CATEGORY_LABELS: Record<TriggerEventCatalogItem["category"], string> = {
  manual: "Manual",
  form: "Forms",
  integration: "Integrations",
  planning: "Planning & Grids",
  api: "API",
};

// revisionId: the Build working revision, whose forms and integrations the
// per-source events name.
export function useTriggerEvents(applicationId: string, revisionId?: string) {
  return useQuery({
    queryKey: ["workflow-trigger-events", applicationId, revisionId],
    queryFn: () => api.listWorkflowTriggerEvents(applicationId, revisionId),
    staleTime: 60_000,
    enabled: !!applicationId,
  });
}

// Token-based styles for the raw inputs/buttons still inline in this file family.
export const inputStyle: React.CSSProperties = {
  width: "100%", padding: "6px 8px", border: "1px solid var(--color-border-strong)", borderRadius: "var(--radius-input)",
  fontSize: 13, outline: "none", boxSizing: "border-box", fontFamily: "inherit",
  background: "var(--color-surface)", color: "var(--color-text)",
};

export const btnPrimary: React.CSSProperties = {
  padding: "7px 14px", borderRadius: "var(--radius-button)", border: "none", background: "var(--color-brand-solid)",
  color: "var(--color-text-inverse)", fontSize: 13, cursor: "pointer", fontWeight: 600,
};

export const btnSecondary: React.CSSProperties = {
  padding: "7px 14px", borderRadius: "var(--radius-button)", border: "1px solid var(--color-border-strong)",
  background: "var(--color-surface)", fontSize: 13, cursor: "pointer", color: "var(--color-text)",
};
