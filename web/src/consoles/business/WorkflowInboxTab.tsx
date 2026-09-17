import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { BadgeCheck, GitFork, Bell, CheckSquare, ChevronUp, ChevronDown, AlertTriangle, Play } from "lucide-react";
import { api, type Task, type WorkflowDefPublic, type DevDimension } from "../../api/client";
import { LoadingState, EmptyState, StatusBadge, Textarea, TextInput, Button, Dialog, Field, InlineAlert } from "../../ui";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { TaskContextSummary } from "../TaskContextSummary";
import { WorkflowStepActions } from "../WorkflowStepActions";

// ── Start a workflow (dialog generated from context_schema) ──────────────────
//
// The workflow's context_schema is the single source of what an instance
// must be scoped by; this dialog renders one control per declared variable —
// a "Dimension member" variable becomes a member picker over /api/dimensions
// (already filtered by the caller's hidden rules, so a user scoped to Canada
// is offered exactly Canada), everything else a plain input. This is how a
// business user DEFINES what they are submitting for approval, instead of
// depending on a per-scope dashboard button frozen at design time. The
// server re-validates everything (ResolveStartContext rejects hidden
// members), so the picker filtering is UX, not the security boundary.
function StartWorkflowDialog({ def, dims, onClose }: { def: WorkflowDefPublic; dims: DevDimension[]; onClose: () => void }) {
  const qc = useQueryClient();
  // Dimension-member fields hold a LIST of selected codes; other fields a
  // single string. One instance is started per combination, because the
  // lock engine scopes an instance to a single member per context key —
  // and separate instances are the right semantics anyway: the approver
  // decides each country on its own, not all-or-nothing.
  const [values, setValues] = useState<Record<string, string>>({});
  const [multi, setMulti] = useState<Record<string, string[]>>({});
  const schema = def.context_schema ?? [];
  const isMember = (v: { data_type?: string }) => v.data_type === "Dimension member";
  const isMetric = (v: { data_type?: string }) => v.data_type === "Metric";
  // Metric options for "Metric" context vars — an approval can scope the
  // crossing of dimension members AND specific metrics; hidden-filtered
  // server-side. Values are metric NAMES (the identity the lock matches).
  const { data: metricOptions = [] } = useQuery({
    queryKey: ["public-metrics"],
    queryFn: () => api.getMetrics(),
    enabled: schema.some(isMetric),
  });

  const combos = (() => {
    let out: Record<string, string>[] = [{}];
    for (const v of schema) {
      const options = isMember(v) || isMetric(v) ? (multi[v.key] ?? []) : [(values[v.key] ?? "").trim()];
      if (options.length === 0 || options.some(o => o === "")) return [];
      out = out.flatMap(c => options.map(o => ({ ...c, [v.key]: o })));
    }
    return out;
  })();
  const MAX_COMBOS = 20;

  const start = useMutation({
    mutationFn: async () => {
      for (const c of combos) {
        await api.startWorkflow(def.id, c);
      }
      return combos.length;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
      qc.invalidateQueries({ queryKey: ["wf-my-history"] });
    },
  });

  const membersFor = (dimensionID?: string) => {
    const dim = dims.find(d => d.id === dimensionID);
    if (!dim) return null;
    const codeByID = new Map(dim.members.map(m => [m.id, m.code]));
    return dim.members.map(m => ({
      code: m.code,
      label: m.label,
      parent_code: m.parent_member_id ? codeByID.get(m.parent_member_id) : undefined,
    }));
  };
  const labelFor = (dimensionID: string | undefined, code: string) =>
    dims.find(d => d.id === dimensionID)?.members.find(m => m.code === code)?.label ?? code;

  return (
    <Dialog open onClose={onClose} title={`Start: ${def.name}`} width={440}>
      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        {def.description && <p className="mvx-admin-muted" style={{ fontSize: 13, margin: 0 }}>{def.description}</p>}
        {schema.length === 0 && (
          <p className="mvx-admin-muted" style={{ fontSize: 13, margin: 0 }}>This workflow needs no additional details.</p>
        )}
        {schema.map(v => {
          const members = isMember(v) ? membersFor(v.dimension_id) : null;
          const chosen = multi[v.key] ?? [];
          return (
            <Field key={v.key} label={v.label || v.key} required>
              {isMetric(v) ? (
                <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                  <div style={{ display: "flex", gap: 6 }}>
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={metricOptions.length === 0 || chosen.length === metricOptions.length}
                      onClick={() => setMulti(s => ({ ...s, [v.key]: metricOptions.map(m => m.name) }))}
                    >
                      Select all ({metricOptions.length})
                    </Button>
                    {chosen.length > 0 && (
                      <Button size="sm" variant="ghost" onClick={() => setMulti(s => ({ ...s, [v.key]: [] }))}>
                        Clear
                      </Button>
                    )}
                  </div>
                  {chosen.length > 0 && (
                    <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                      {chosen.map(name => (
                        <StatusBadge key={name} tone="brand">
                          {metricOptions.find(m => m.name === name)?.label || name}
                          <button
                            type="button"
                            aria-label={`Remove ${name}`}
                            onClick={() => setMulti(s => ({ ...s, [v.key]: (s[v.key] ?? []).filter(c => c !== name) }))}
                            style={{ marginLeft: 4, border: "none", background: "none", cursor: "pointer", color: "inherit", padding: 0 }}
                          >
                            ×
                          </button>
                        </StatusBadge>
                      ))}
                    </div>
                  )}
                  {/* Same searchable picker as dimension members — a flat
                      metric list is just a hierarchy with no parents. */}
                  <HierarchicalMemberSelect
                    id={`wf-ctx-${v.key}`}
                    ariaLabel={v.label || v.key}
                    members={metricOptions.map(m => ({ code: m.name, label: m.label || m.name }))}
                    value=""
                    placeholder={chosen.length ? "Add another metric…" : "Select a metric…"}
                    isSelectable={m => !chosen.includes(m.code)}
                    onChange={name => {
                      if (!name) return;
                      setMulti(s => ({ ...s, [v.key]: [...(s[v.key] ?? []), name] }));
                    }}
                  />
                </div>
              ) : isMember(v) ? (
                members ? (
                  <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                    <div style={{ display: "flex", gap: 6 }}>
                      {(() => {
                        // Leaves only: selecting a parent already covers its
                        // whole subtree via the lock's ancestor cascade, so
                        // "all" means every leaf, without redundant combos.
                        const leaves = members.filter(m => !members.some(x => x.parent_code === m.code)).map(m => m.code);
                        return (
                          <Button
                            size="sm"
                            variant="ghost"
                            disabled={leaves.length === 0 || leaves.every(c => chosen.includes(c))}
                            onClick={() => setMulti(s => ({ ...s, [v.key]: leaves }))}
                          >
                            Select all ({leaves.length})
                          </Button>
                        );
                      })()}
                      {chosen.length > 0 && (
                        <Button size="sm" variant="ghost" onClick={() => setMulti(s => ({ ...s, [v.key]: [] }))}>
                          Clear
                        </Button>
                      )}
                    </div>
                    {chosen.length > 0 && (
                      <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                        {chosen.map(code => (
                          <StatusBadge key={code} tone="brand">
                            {labelFor(v.dimension_id, code)}
                            <button
                              type="button"
                              aria-label={`Remove ${code}`}
                              onClick={() => setMulti(s => ({ ...s, [v.key]: (s[v.key] ?? []).filter(c => c !== code) }))}
                              style={{ marginLeft: 4, border: "none", background: "none", cursor: "pointer", color: "inherit", padding: 0 }}
                            >
                              ×
                            </button>
                          </StatusBadge>
                        ))}
                      </div>
                    )}
                    <HierarchicalMemberSelect
                      id={`wf-ctx-${v.key}`}
                      ariaLabel={v.label || v.key}
                      members={members}
                      value=""
                      placeholder={chosen.length ? "Add another…" : "Select…"}
                      isSelectable={m => !chosen.includes(m.code)}
                      onChange={code => {
                        if (!code) return;
                        setMulti(s => ({ ...s, [v.key]: [...(s[v.key] ?? []), code] }));
                      }}
                    />
                  </div>
                ) : (
                  <InlineAlert tone="warning">
                    The dimension this field points at is not in the current revision — ask a developer to update the workflow.
                  </InlineAlert>
                )
              ) : (
                <TextInput
                  type={v.data_type === "Number" ? "number" : "text"}
                  value={values[v.key] ?? ""}
                  onChange={e => setValues(s => ({ ...s, [v.key]: e.target.value }))}
                />
              )}
            </Field>
          );
        })}
        {combos.length > MAX_COMBOS && (
          <InlineAlert tone="warning">
            {combos.length} requests would be started — reduce the selection to at most {MAX_COMBOS}.
          </InlineAlert>
        )}
        {start.isSuccess ? (
          <>
            <InlineAlert tone="success">
              {start.data === 1
                ? "Submitted — the request is now awaiting its approver, and the scoped data is locked until a decision is made."
                : `${start.data} requests submitted — each is awaiting its approver, and each scope is locked until its own decision.`}
            </InlineAlert>
            <Button variant="primary" onClick={onClose}>Done</Button>
          </>
        ) : (
          <>
            {start.isError && <div className="mvx-admin-error">{(start.error as Error).message}</div>}
            <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
              <Button onClick={onClose}>Cancel</Button>
              <Button
                variant="primary"
                disabled={combos.length === 0 || combos.length > MAX_COMBOS || start.isPending}
                loading={start.isPending}
                loadingLabel="Submitting…"
                onClick={() => start.mutate()}
              >
                {combos.length > 1 ? `Submit ${combos.length} requests` : "Submit"}
              </Button>
            </div>
          </>
        )}
      </div>
    </Dialog>
  );
}

export function StartWorkflowSection() {
  const { data: defs = [] } = useQuery({ queryKey: ["wf-defs"], queryFn: api.listWorkflowDefinitions });
  const { data: dims = [] } = useQuery({ queryKey: ["public-dims"], queryFn: api.getDimensions });
  const [active, setActive] = useState<WorkflowDefPublic | null>(null);

  const manual = defs.filter(d => d.trigger_event === "manual");
  if (manual.length === 0) return null;

  return (
    <div style={{ marginBottom: 16 }}>
      <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>Start a workflow</div>
      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        {manual.map(d => (
          <div key={d.id} className="mvx-panel" style={{ padding: "10px 14px", display: "flex", alignItems: "center", gap: 12 }}>
            <div style={{ flex: 1 }}>
              <div style={{ fontWeight: 600, fontSize: 13 }}>{d.name}</div>
              {d.description && <div className="mvx-admin-muted" style={{ fontSize: 12 }}>{d.description}</div>}
            </div>
            <Button size="sm" leadingIcon={<Play size={13} />} onClick={() => setActive(d)}>Start</Button>
          </div>
        ))}
      </div>
      {active && <StartWorkflowDialog def={active} dims={dims as DevDimension[]} onClose={() => setActive(null)} />}
    </div>
  );
}

export function StepTypeIcon({ type, size = 16 }: { type: string; size?: number }) {
  const props = { size, "aria-hidden": true as const, style: { flexShrink: 0, color: "var(--color-text-muted)" } };
  switch (type) {
    case "approval": return <BadgeCheck {...props} />;
    case "condition": return <GitFork {...props} />;
    case "notification": return <Bell {...props} />;
    default: return <CheckSquare {...props} />;
  }
}

export function WorkflowInbox() {
  const qc = useQueryClient();
  const { data: tasks = [], isLoading } = useQuery({
    queryKey: ["tasks"],
    queryFn: api.getTasks,
    refetchInterval: 10_000,
  });

  const complete = useMutation({
    mutationFn: ({ stepId, decision, comment }: { stepId: string; decision: string; comment: string }) =>
      api.completeTask(stepId, decision, comment),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tasks"] }),
  });

  const [comment, setComment] = useState<Record<string, string>>({});
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  if (isLoading) return <LoadingState label="Loading tasks…" />;
  if (tasks.length === 0) {
    return (
      <div>
        <StartWorkflowSection />
        <EmptyState label="No pending tasks. You're all caught up." />
      </div>
    );
  }

  const now = new Date();
  const isDueSoon = (dueAt?: string) => {
    if (!dueAt) return false;
    return new Date(dueAt).getTime() - now.getTime() < 24 * 60 * 60 * 1000;
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <StartWorkflowSection />
      {tasks.map((t: Task) => {
        const isOpen = expanded[t.id] ?? true;
        const commentVal = comment[t.id] ?? "";
        const canSubmit = !t.required_comment || commentVal.trim().length > 0;
        const overdue = t.due_at && new Date(t.due_at) < now;

        return (
          <div key={t.id} className="mvx-admin-object" style={overdue ? { borderColor: "var(--color-danger-border)" } : undefined}>
            {/* Card header */}
            <div
              className="mvx-admin-object__header"
              onClick={() => setExpanded(e => ({ ...e, [t.id]: !isOpen }))}
              style={{ cursor: "pointer", borderBottom: isOpen ? undefined : "none" }}
            >
              <StepTypeIcon type={t.step_type} size={18} />
              <div className="mvx-admin-object__title">
                <div className="mvx-admin-object__name">{t.step_name || t.step_def_id}</div>
                <div className="mvx-admin-object__meta">
                  {t.workflow_name}
                  {t.requested_by && <span> · requested by {t.requested_by}</span>}
                  {t.context?.revision && <span> · {t.context.revision}</span>}
                </div>
              </div>
              <div style={{ display: "flex", flexDirection: "column", alignItems: "flex-end", gap: 3, flexShrink: 0 }}>
                {t.due_at && (
                  <StatusBadge tone={overdue ? "danger" : isDueSoon(t.due_at) ? "warning" : "neutral"}>
                    {overdue && <AlertTriangle size={11} aria-hidden="true" style={{ marginRight: 3, verticalAlign: -1 }} />}
                    {overdue ? "Overdue" : "Due"} {new Date(t.due_at).toLocaleDateString()}
                  </StatusBadge>
                )}
                <span className="mvx-admin-muted">{new Date(t.created_at).toLocaleDateString()}</span>
              </div>
              {isOpen
                ? <ChevronUp size={14} aria-hidden="true" style={{ color: "var(--color-text-subtle)" }} />
                : <ChevronDown size={14} aria-hidden="true" style={{ color: "var(--color-text-subtle)" }} />}
            </div>

            {/* Expanded body */}
            {isOpen && (
              <div className="mvx-admin-object__body">
                {t.rework_count ? (
                  <InlineAlert tone="warning"><strong>Sent back for rework ({t.rework_count})</strong> {t.rework_note?.replace(/^Sent back for rework \(\d+\) /, "")}</InlineAlert>
                ) : null}
                {t.instructions && (
                  <p style={{ fontSize: 13, margin: 0, lineHeight: 1.5 }}>{t.instructions}</p>
                )}

                {/* Context summary — prefer the server-resolved display names
                    ("Canada (CA)", metric names) over raw codes; the raw
                    context map remains the fallback for older payloads. */}
                <TaskContextSummary contextDisplay={t.context_display} context={t.context} />

                <Textarea
                  placeholder={t.required_comment ? "Comment required" : "Comment (optional)"}
                  value={commentVal}
                  error={Boolean(t.required_comment && !commentVal.trim())}
                  onChange={(e) => setComment((c) => ({ ...c, [t.id]: e.target.value }))}
                  rows={2}
                  style={{ resize: "none" }}
                />
                {t.required_comment && !commentVal.trim() && (
                  <div className="mvx-admin-error">Comment is required.</div>
                )}

                <WorkflowStepActions
                  stepType={t.step_type}
                  completionLabel={t.completion_label}
                  condition={t.condition}
                  disabled={complete.isPending || !canSubmit}
                  onDecide={decision => complete.mutate({ stepId: t.id, decision, comment: commentVal })}
                />
                {complete.isError && (
                  <div className="mvx-admin-error">{String(complete.error)}</div>
                )}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
