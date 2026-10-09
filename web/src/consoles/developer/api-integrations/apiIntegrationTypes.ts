// Shared shapes and defaults for the REST API visual constructor. The typed
// DTOs live in api/client.ts; this module holds wizard-local structure only.
import type { ApiIntegrationConfig, ApiModelSource, ApiSchedule, ApiSFTPSource } from "../../../api/client";

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

export function defaultSFTPSource(): ApiSFTPSource {
  return { host: "", select: "fixed", path: "" };
}

export function defaultModelSource(): ApiModelSource {
  return { model_id: "", grid: "" };
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
    pr: c.protocol, s: c.sftp, m: c.model,
  });
}

// sshFingerprint renders an authorized_keys line's SHA256 fingerprint the way
// ssh-keygen -l prints it ("SHA256:…"), so a trusted key can be compared with
// the server's. Empty when the key does not decode.
export async function sshFingerprint(authorizedKey: string): Promise<string> {
  const blob = authorizedKey.trim().split(/\s+/)[1] ?? "";
  try {
    const raw = Uint8Array.from(atob(blob), c => c.charCodeAt(0));
    const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", raw));
    return "SHA256:" + btoa(String.fromCharCode(...sum)).replace(/=+$/, "");
  } catch {
    return "";
  }
}
