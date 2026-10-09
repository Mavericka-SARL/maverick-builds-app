// The credential fields of an AI proposal's sign-in connection steps, as
// internal/aiassistant/tools_connector.go defines them (SecretFields,
// OptionalSecretFields, PlainFields): what the confirmation card asks the
// developer for. The secret parts never come from the assistant.
import type { AIProposalStep } from "../../api/client";

export interface CredentialField {
  key: string;
  label: string;
  secret: boolean;
  optional?: boolean;
  multiline?: boolean;
}

const FIELDS: Record<string, CredentialField[]> = {
  api_key: [{ key: "value", label: "API key", secret: true }],
  bearer: [{ key: "token", label: "Bearer token", secret: true }],
  basic: [{ key: "username", label: "Username", secret: false }, { key: "password", label: "Password", secret: true }],
  oauth2_client_credentials: [{ key: "client_id", label: "Client ID", secret: false }, { key: "client_secret", label: "Client secret", secret: true }],
  oauth2_authorization_code: [{ key: "client_secret", label: "Client secret", secret: true }],
  ssh_key: [
    { key: "username", label: "Username", secret: false },
    { key: "private_key", label: "Private key", secret: true, multiline: true },
    { key: "passphrase", label: "Key passphrase", secret: true, optional: true },
  ],
};

/** A step of the proposal that needs the developer's credential, with its fields. */
export interface CredentialStep {
  number: number; // 1-based, as the confirmation keys it
  name: string;
  authType: string;
  fields: CredentialField[];
  prefill: Record<string, string>;
}

export function credentialSteps(steps: AIProposalStep[]): CredentialStep[] {
  const out: CredentialStep[] = [];
  steps.forEach((s, i) => {
    const p = (s.params ?? {}) as Record<string, unknown>;
    const asks = s.tool === "create_connection" || (s.tool === "update_connection" && p.replace_credential === true);
    const authType = typeof p.auth_type === "string" ? p.auth_type : "";
    const fields = FIELDS[authType];
    if (!asks || !fields) return;
    const cred = (p.credential ?? {}) as Record<string, unknown>;
    const prefill: Record<string, string> = {};
    for (const f of fields) if (!f.secret && typeof cred[f.key] === "string") prefill[f.key] = cred[f.key] as string;
    out.push({ number: i + 1, name: typeof p.name === "string" && p.name ? p.name : `step ${i + 1}`, authType, fields, prefill });
  });
  return out;
}

/** Whether every required secret of every step is filled in. */
export function credentialsComplete(steps: CredentialStep[], values: Record<number, Record<string, string>>): boolean {
  return steps.every(s => s.fields.every(f => !f.secret || f.optional || (values[s.number]?.[f.key] ?? "").trim() !== ""));
}
