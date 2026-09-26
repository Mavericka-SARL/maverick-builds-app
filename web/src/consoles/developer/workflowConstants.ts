import { useQuery } from "@tanstack/react-query";
import type { DesignTone } from "../../ui";
import { api } from "../../api/client";
import type { StepType, TriggerEventCatalogItem } from "../../api/client";

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

export function useTriggerEvents(applicationId: string) {
  return useQuery({
    queryKey: ["workflow-trigger-events", applicationId],
    queryFn: () => api.listWorkflowTriggerEvents(applicationId),
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
