import { CONTROL_PLANE, type AdminTenant } from "../../api/client";
import { Field, Select } from "../../ui";
import { ALL_PEOPLE } from "./people";

/**
 * Whose people the platform console's Users tab shows. It is one list across
 * every database, each row naming the one it lives in; this narrows it. Shown
 * only when tenants with a database of their own exist.
 */
export function UsersScopePicker({
  tenants,
  value,
  onChange,
}: {
  tenants: AdminTenant[];
  value: string;
  onChange: (scope: string) => void;
}) {
  const dedicated = tenants.filter((t) => t.dedicated);
  if (dedicated.length === 0) return null;
  return (
    <div style={{ marginBottom: 16, maxWidth: 420 }} data-testid="users-scope">
      <Field label="People of" description="Everyone, or one database: the control plane, or a tenant with its own.">
        <Select value={value} onChange={(e) => onChange(e.target.value)} aria-label="People of">
          <option value={ALL_PEOPLE}>Everyone</option>
          <option value={CONTROL_PLANE}>Platform and shared-database tenants</option>
          {dedicated.map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
        </Select>
      </Field>
    </div>
  );
}
