import { useQuery } from "@tanstack/react-query";
import { api } from "../../api/client";

// Tenant roles offered alongside named business roles. Limited to
// business_admin/business_user — the only roles with a Workflow Inbox
// surface (Business/Business Admin console) to act on a step or start a
// workflow from; developer/tenant_admin/platform_admin have no such tab, so
// assigning a step to them would create a task nobody can ever open.
const ASSIGNABLE_TENANT_ROLES = ["business_admin", "business_user"] as const;

/**
 * Roles to tick: the tenant roles and the application's named business
 * roles, by name — what a step's assignee_roles and a workflow's
 * start_roles hold. A name that matches neither (a role since renamed or
 * deleted) stays listed, so it can be seen and unticked.
 */
export function RolePicker({ applicationId, value, onChange }: {
  applicationId: string;
  value: string[];
  onChange: (roles: string[]) => void;
}) {
  const { data: businessRoles = [] } = useQuery({
    queryKey: ["workflow-roles", applicationId],
    queryFn: () => api.listWorkflowRoles(applicationId),
    staleTime: 60_000,
    enabled: !!applicationId,
  });

  const assigned = value;
  const namedRoleNames = businessRoles.map(r => r.name);
  // Anything assigned that matches neither a tenant role nor
  // a known named role — e.g. a business_role that was since
  // renamed/deleted — would otherwise vanish from view with no
  // way to tell it's still set. Surface it instead of hiding it.
  const unknown = assigned.filter(r => !ASSIGNABLE_TENANT_ROLES.includes(r as typeof ASSIGNABLE_TENANT_ROLES[number]) && !namedRoleNames.includes(r));

  const toggle = (name: string) => {
    const checked = assigned.includes(name);
    onChange(checked ? assigned.filter(x => x !== name) : [...assigned, name]);
  };

  const roleCheckbox = (key: string, label: string) => {
    const checked = assigned.includes(key);
    return (
      <label key={key} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13, cursor: "pointer" }}>
        <input type="checkbox" checked={checked} onChange={() => toggle(key)} />
        {label}
      </label>
    );
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10, padding: "8px", background: "var(--color-surface-faint)", borderRadius: 6, border: "1px solid var(--color-border)" }}>
      <div>
        <div style={{ fontSize: 10, fontWeight: 700, color: "var(--color-disabled)", textTransform: "uppercase", letterSpacing: "0.04em", marginBottom: 4 }}>
          Tenant roles
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          {ASSIGNABLE_TENANT_ROLES.map(role => roleCheckbox(role, role))}
        </div>
      </div>
      {businessRoles.length > 0 && (
        <div>
          <div style={{ fontSize: 10, fontWeight: 700, color: "var(--color-disabled)", textTransform: "uppercase", letterSpacing: "0.04em", marginBottom: 4 }}>
            Named roles
          </div>
          <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
            {businessRoles.map(r => roleCheckbox(r.name, r.name))}
          </div>
        </div>
      )}
      {unknown.length > 0 && (
        <div>
          <div style={{ fontSize: 10, fontWeight: 700, color: "var(--color-danger)", textTransform: "uppercase", letterSpacing: "0.04em", marginBottom: 4 }}>
            Assigned but unrecognized
          </div>
          <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
            {unknown.map(role => roleCheckbox(role, role))}
          </div>
        </div>
      )}
      {businessRoles.length === 0 && (
        <div style={{ fontSize: 11, color: "var(--color-disabled)" }}>
          Need a named team (e.g. "Budget Approvers")? A business admin creates one in Business Admin › Roles.
        </div>
      )}
    </div>
  );
}
