// Shared shapes and defaults for the REST API visual constructor. The typed
// DTOs live in api/client.ts; this module holds wizard-local structure only.
import type { ApiIntegrationConfig, ApiSchedule } from "../../../api/client";

export const WIZARD_STEPS = [
  { id: "basics", label: "Basics" },
  { id: "request", label: "Request" },
  { id: "auth", label: "Authentication" },
  { id: "response", label: "Test & response" },
  { id: "mapping", label: "Mapping" },
  { id: "schedule", label: "Run & schedule" },
] as const;

export type WizardStepID = (typeof WIZARD_STEPS)[number]["id"];

export function defaultConfig(): ApiIntegrationConfig {
  return {
    kind: "rest_api/v1",
    direction: "pull",
    target_type: "grid",
    target_id: "",
    import_mode: "incremental",
    request: { method: "GET", url: "", query: [], headers: [], body_mode: "none" },
    auth: { type: "none" },
    response: { format: "json", records_path: "" },
    pagination: { mode: "none" },
    mapping: { fields: [] },
    limits: {},
  };
}

export function defaultSchedule(): ApiSchedule {
  return { kind: "manual", enabled: false, timezone: "UTC", overlap_policy: "skip", misfire_policy: "skip" };
}

// The closed template-variable namespace the picker offers (spec Step 2).
export const TEMPLATE_VARIABLES = [
  "{{context.application_id}}",
  "{{context.model_id}}",
  "{{context.revision_id}}",
  "{{run.id}}",
  "{{run.started_at}}",
  "{{page.number}}",
  "{{page.cursor}}",
] as const;

export const TRANSFORM_KINDS = [
  { id: "", label: "None" },
  { id: "trim", label: "Trim" },
  { id: "to_string", label: "To string" },
  { id: "to_number", label: "To number" },
  { id: "to_boolean", label: "To boolean" },
  { id: "to_date", label: "To date (ISO)" },
  { id: "date_format", label: "Date format…" },
  { id: "default", label: "Default value…" },
  { id: "lookup", label: "Lookup table…" },
] as const;

// Request-critical fields: editing any of these invalidates the previous
// successful test (the server enforces via ConfigHash; this mirrors it for
// immediate UI feedback).
export function requestCriticalKey(c: ApiIntegrationConfig): string {
  return JSON.stringify({
    d: c.direction, tt: c.target_type, t: c.target_id,
    r: c.request, a: c.auth, re: c.response, p: c.pagination,
  });
}
