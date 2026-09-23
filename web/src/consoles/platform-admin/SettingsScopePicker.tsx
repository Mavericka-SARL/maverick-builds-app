import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { setScopedTenant, type AdminTenant } from "../../api/client";
import { Field, InlineAlert, Select } from "../../ui";

/**
 * At platform scope, whose settings the per-tenant tabs show. The platform
 * administrator has no tenant of their own, so every one of these screens
 * needs to be told: one tenant, or — for delivery, retention and the AI key,
 * on an edition that has them — the deployment's own row, the defaults a
 * tenant inherits until it sets its own. The choice is sent as X-Tenant-Id
 * on every call the tab makes (setScopedTenant), and the tabs' caches are
 * dropped on each change so no tenant's values linger on another's screen.
 *
 * The tenant scope needs none of this: the server knows the tenant admin's
 * own tenant.
 */

/** Query keys of the tabs that show per-tenant settings. */
const SCOPED_QUERY_KEYS = ["notification-settings", "audit-settings", "tenant-ai-settings", "sso-settings", "scim-tokens", "branding", "admin-audit"];

export const DEPLOYMENT_SCOPE = "";

export function SettingsScopePicker({
  tenants,
  value,
  onChange,
  deploymentRow,
}: {
  tenants: AdminTenant[];
  value: string;
  onChange: (tenantId: string) => void;
  /** Whether this setting has a deployment-wide row to offer. */
  deploymentRow: boolean;
}) {
  const qc = useQueryClient();
  // The choice lives in the API client for as long as the picker is on
  // screen; leaving the platform section takes it away again.
  useEffect(() => {
    setScopedTenant(value);
    return () => setScopedTenant("");
  }, [value]);
  useEffect(() => {
    for (const key of SCOPED_QUERY_KEYS) void qc.removeQueries({ queryKey: [key] });
  }, [qc, value]);

  const options = tenants.map((t) => ({ id: t.id, label: t.name }));
  const chosen = value === DEPLOYMENT_SCOPE ? (deploymentRow ? DEPLOYMENT_SCOPE : "") : value;
  return (
    <div style={{ marginBottom: 16, maxWidth: 420 }} data-testid="settings-scope">
      <Field label="Settings of" description={deploymentRow ? "The deployment's own row is what a tenant inherits until it sets its own (enterprise)." : "This setting belongs to a tenant; choose one."}>
        <Select value={chosen} onChange={(e) => onChange(e.target.value)} aria-label="Settings scope">
          {deploymentRow ? <option value={DEPLOYMENT_SCOPE}>Deployment defaults</option> : <option value="" disabled>Choose a tenant…</option>}
          {options.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
        </Select>
      </Field>
      {!deploymentRow && value === DEPLOYMENT_SCOPE && (
        <InlineAlert tone="info">Identity settings are always one tenant&apos;s own. Choose a tenant above.</InlineAlert>
      )}
    </div>
  );
}
