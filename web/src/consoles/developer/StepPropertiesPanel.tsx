import { useQuery } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { WorkflowStepDef } from "../../api/client";
import { StepTypeIcon, Field } from "./WorkflowShared";
import { STEP_TYPE_LABELS, inputStyle } from "./workflowConstants";

const CONDITION_OPERATORS = [
  { value: "equals", label: "equals" },
  { value: "not_equals", label: "not equals" },
  { value: "greater_than", label: "greater than" },
  { value: "greater_than_or_equal", label: ">=" },
  { value: "less_than", label: "less than" },
  { value: "less_than_or_equal", label: "<=" },
  { value: "contains", label: "contains" },
  { value: "is_empty", label: "is empty" },
  { value: "is_not_empty", label: "is not empty" },
];

// ── ParallelRoutesEditor ──────────────────────────────────────────────────────
// Used by task and notification steps. Shows a list of outgoing branches;
// adding a second branch enables parallel fan-out.

function getBranches(routes: Record<string, string> | undefined): { key: string; value: string }[] {
  const r = routes ?? {};
  // Collect "next" or "branch-N" keys (not approval/condition keys).
  const special = new Set(["approve", "reject", "true", "false"]);
  return Object.entries(r)
    .filter(([k]) => !special.has(k))
    .map(([k, v]) => ({ key: k, value: v }));
}

function ParallelRoutesEditor({ step, routeOptions, onChange }: {
  step: WorkflowStepDef;
  routeOptions: { value: string; label: string }[];
  onChange: (patch: Partial<WorkflowStepDef>) => void;
}) {
  const branches = getBranches(step.routes);

  const setBranch = (idx: number, target: string) => {
    const updated = [...branches];
    updated[idx] = { ...updated[idx], value: target };
    const newRoutes: Record<string, string> = {};
    for (const b of updated) newRoutes[b.key] = b.value;
    onChange({ routes: newRoutes });
  };

  const addBranch = () => {
    const newRoutes: Record<string, string> = {};
    // If still using the simple "next" key, rename it to "branch-0" first.
    if (branches.length === 1 && branches[0].key === "next") {
      newRoutes["branch-0"] = branches[0].value;
      newRoutes["branch-1"] = "";
    } else {
      for (const b of branches) newRoutes[b.key] = b.value;
      newRoutes[`branch-${branches.length}`] = "";
    }
    onChange({ routes: newRoutes });
  };

  const removeBranch = (key: string) => {
    const remaining = branches.filter(b => b.key !== key);
    const newRoutes: Record<string, string> = {};
    if (remaining.length === 1) {
      // Simplify back to "next".
      newRoutes["next"] = remaining[0].value;
    } else {
      // Renumber to keep keys tidy.
      remaining.forEach((b, i) => { newRoutes[`branch-${i}`] = b.value; });
    }
    onChange({ routes: newRoutes });
  };

  const isParallel = branches.length > 1;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 6 }}>
        <span style={{ fontSize: 12, fontWeight: 600, color: "var(--color-text-strong)" }}>
          {isParallel ? "Parallel branches" : "Next step"}
        </span>
        <button
          onClick={addBranch}
          style={{ fontSize: 11, color: "var(--color-selected)", background: "none", border: "none", cursor: "pointer", padding: "2px 6px" }}
          title="Add parallel branch"
        >
          + parallel
        </button>
      </div>
      {branches.map((branch, idx) => (
        <div key={branch.key} style={{ display: "flex", gap: 4, alignItems: "center", marginBottom: 6 }}>
          {isParallel && (
            <span style={{ fontSize: 11, color: "var(--color-disabled)", width: 60, flexShrink: 0 }}>
              Branch {idx + 1}
            </span>
          )}
          <select
            value={branch.value}
            onChange={e => setBranch(idx, e.target.value)}
            style={{ ...inputStyle, flex: 1 }}
          >
            {routeOptions.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
          {isParallel && (
            <button
              onClick={() => removeBranch(branch.key)}
              style={{ background: "none", border: "none", cursor: "pointer", color: "var(--color-disabled)", fontSize: 14, padding: "0 4px" }}
              title="Remove branch"
            >✕</button>
          )}
        </div>
      ))}
      {branches.length === 0 && (
        <select
          value=""
          onChange={e => onChange({ routes: { next: e.target.value } })}
          style={inputStyle}
        >
          {routeOptions.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>
      )}
    </div>
  );
}

// Platform roles offered as step assignees alongside named business roles.
// Limited to business_admin/business_user — the only roles with a Workflow
// Inbox surface (Business/Business Admin console) to actually act on a step;
// developer/tenant_admin/platform_admin have no such tab, so assigning a
// step to them would create a task nobody can ever open.
const ASSIGNABLE_PLATFORM_ROLES = ["business_admin", "business_user"] as const;

export function StepPropertiesPanel({ step, allSteps, applicationId, onChange, onDelete, onClose }: {
  step: WorkflowStepDef;
  allSteps: WorkflowStepDef[];
  applicationId: string;
  onChange: (patch: Partial<WorkflowStepDef>) => void;
  onDelete: () => void;
  onClose: () => void;
}) {
  const { data: businessRoles = [] } = useQuery({
    queryKey: ["workflow-roles", applicationId],
    queryFn: () => api.listWorkflowRoles(applicationId),
    staleTime: 60_000,
    enabled: !!applicationId,
  });
  const routeOptions = [
    { value: "", label: "— select —" },
    ...allSteps.filter(s => s.id !== step.id).map(s => ({ value: s.id, label: s.name })),
    { value: "end-completed", label: "End: Completed" },
    { value: "end-rejected", label: "End: Rejected" },
    { value: "end-cancelled", label: "End: Cancelled" },
  ];

  const setRoute = (key: string, val: string) => {
    onChange({ routes: { ...step.routes, [key]: val } });
  };

  const setRole = (roles: string[]) => onChange({ assignee_roles: roles });

  return (
    <div style={{ padding: "14px" }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 16, paddingBottom: 12, borderBottom: "1px solid var(--color-surface-muted)" }}>
        <StepTypeIcon type={step.type} size={20} />
        <div style={{ flex: 1 }}>
          <div style={{ fontSize: 14, fontWeight: 700, color: "var(--color-text)" }}>{STEP_TYPE_LABELS[step.type]} Step</div>
          <div style={{ fontSize: 11, color: "var(--color-disabled)" }}>Configure this step</div>
        </div>
        <button
          onClick={onDelete}
          title="Delete step"
          style={{ background: "none", border: "1px solid var(--color-danger-border)", borderRadius: 6, cursor: "pointer", color: "var(--color-danger)", fontSize: 12, padding: "4px 8px" }}
        >
          Delete
        </button>
        <button
          onClick={onClose}
          title="Close panel"
          style={{ background: "none", border: "none", cursor: "pointer", color: "var(--color-disabled)", fontSize: 16, padding: "4px 6px", lineHeight: 1 }}
        >
          ✕
        </button>
      </div>

      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <Field label="Step name">
          <input value={step.name} onChange={e => onChange({ name: e.target.value })} style={inputStyle} />
        </Field>
        <Field label="Instructions">
          <textarea value={step.instructions ?? ""} onChange={e => onChange({ instructions: e.target.value })} rows={3} style={{ ...inputStyle, resize: "vertical" as const }} />
        </Field>

        {(step.type === "task" || step.type === "approval") && (
          <>
            <Field label={step.type === "approval" ? "Approver roles" : "Assignee roles"}>
              {(() => {
                const assigned = step.assignee_roles ?? [];
                const namedRoleNames = businessRoles.map(r => r.name);
                // Anything assigned that matches neither a platform role nor
                // a known named role — e.g. a business_role that was since
                // renamed/deleted — would otherwise vanish from view with no
                // way to tell it's still set. Surface it instead of hiding it.
                const unknown = assigned.filter(r => !ASSIGNABLE_PLATFORM_ROLES.includes(r as typeof ASSIGNABLE_PLATFORM_ROLES[number]) && !namedRoleNames.includes(r));

                const toggle = (name: string) => {
                  const checked = assigned.includes(name);
                  setRole(checked ? assigned.filter(x => x !== name) : [...assigned, name]);
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
                        Platform roles
                      </div>
                      <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                        {ASSIGNABLE_PLATFORM_ROLES.map(role => roleCheckbox(role, role))}
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
                        Need a named team (e.g. "Budget Approvers")? Create one in Admin → Roles.
                      </div>
                    )}
                  </div>
                );
              })()}
            </Field>
            <Field label="SLA hours">
              <input
                type="number"
                value={step.sla_hours ?? ""}
                onChange={e => onChange({ sla_hours: e.target.value ? Number(e.target.value) : undefined })}
                placeholder="48"
                style={inputStyle}
              />
            </Field>
            <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 13 }}>
              <input
                type="checkbox"
                checked={step.required_comment ?? false}
                onChange={e => onChange({ required_comment: e.target.checked })}
              />
              Require comment
            </label>
          </>
        )}

        {step.type === "task" && (
          <>
            <Field label="Completion button label">
              <input value={step.completion_label ?? ""} onChange={e => onChange({ completion_label: e.target.value })} placeholder="Complete" style={inputStyle} />
            </Field>
            <ParallelRoutesEditor step={step} routeOptions={routeOptions} onChange={onChange} />
          </>
        )}

        {step.type === "approval" && (
          <>
            <Field label="On approve →">
              <select value={step.routes?.approve ?? ""} onChange={e => setRoute("approve", e.target.value)} style={inputStyle}>
                {routeOptions.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
              </select>
            </Field>
            <Field label="On reject →">
              <select value={step.routes?.reject ?? ""} onChange={e => setRoute("reject", e.target.value)} style={inputStyle}>
                {routeOptions.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
              </select>
            </Field>
          </>
        )}

        {step.type === "condition" && (
          <>
            <div style={{ fontSize: 12, fontWeight: 600, color: "var(--color-text-quiet)", textTransform: "uppercase", letterSpacing: "0.05em" }}>Condition</div>
            <Field label="Context field">
              <input
                value={step.condition?.left ?? ""}
                onChange={e => onChange({ condition: { ...step.condition!, left: e.target.value } })}
                placeholder="e.g. amount"
                style={inputStyle}
              />
            </Field>
            <Field label="Operator">
              <select
                value={step.condition?.operator ?? "equals"}
                onChange={e => onChange({ condition: { ...step.condition!, operator: e.target.value } })}
                style={inputStyle}>
                {CONDITION_OPERATORS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
              </select>
            </Field>
            <Field label="Value">
              <input
                value={String(step.condition?.right ?? "")}
                onChange={e => onChange({ condition: { ...step.condition!, right: e.target.value } })}
                placeholder="e.g. 10000"
                style={inputStyle}
              />
            </Field>
            <Field label="If true →">
              <select value={step.routes?.true ?? ""} onChange={e => setRoute("true", e.target.value)} style={inputStyle}>
                {routeOptions.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
              </select>
            </Field>
            <Field label="If false →">
              <select value={step.routes?.false ?? ""} onChange={e => setRoute("false", e.target.value)} style={inputStyle}>
                {routeOptions.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
              </select>
            </Field>
          </>
        )}

        {step.type === "notification" && (
          <>
            {!step.notification && (
              <div style={{ fontSize: 12, color: "var(--color-draft)", background: "var(--color-surface)eb", border: "1px solid var(--color-warning-border)", borderRadius: 6, padding: "8px 10px" }}>
                Not configured yet — this step will complete without notifying anyone until a recipient is set below.
              </div>
            )}
            <Field label="Recipient">
              <select
                value={step.notification?.recipient_type ?? "requester"}
                onChange={e => onChange({ notification: { ...step.notification, recipient_type: e.target.value as "role" | "requester" } })}
                style={inputStyle}>
                <option value="requester">Requester</option>
                <option value="role">Specific role</option>
              </select>
            </Field>
            {(step.notification?.recipient_type ?? "requester") === "role" && (
              <Field label="Recipient role">
                <input
                  value={step.notification?.recipient_role ?? ""}
                  onChange={e => onChange({ notification: { ...step.notification!, recipient_role: e.target.value } })}
                  placeholder="e.g. finance"
                  style={inputStyle}
                />
              </Field>
            )}
            <Field label="Subject">
              <input
                value={step.notification?.subject ?? ""}
                onChange={e => onChange({ notification: { ...step.notification!, subject: e.target.value } })}
                style={inputStyle}
              />
            </Field>
            <Field label="Message">
              <textarea
                value={step.notification?.message ?? ""}
                onChange={e => onChange({ notification: { ...step.notification!, message: e.target.value } })}
                rows={3}
                style={{ ...inputStyle, resize: "vertical" as const }}
              />
            </Field>
            <ParallelRoutesEditor step={step} routeOptions={routeOptions} onChange={onChange} />
          </>
        )}

        {step.type === "join" && (
          <Field label="Next step">
            <select value={step.routes?.next ?? ""} onChange={e => onChange({ routes: { ...step.routes, next: e.target.value } })} style={inputStyle}>
              {routeOptions.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
            </select>
          </Field>
        )}
      </div>
    </div>
  );
}
