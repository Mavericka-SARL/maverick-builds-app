import React, { useState, useEffect, useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, Plus, Pencil, Trash2, X } from "lucide-react";
import { api, type Task, type HistoryInstance, type BARole, type BAUser, type BAAvailableItem, type UserAccessRule } from "../../api/client";
import { TaskContextSummary } from "../TaskContextSummary";
import { WorkflowStepActions } from "../WorkflowStepActions";
import {
  Button,
  IconButton,
  TextInput,
  Select,
  Checkbox,
  StatusBadge,
  LoadingState,
  EmptyState,
  Toolbar,
  ToolbarGroup,
  SearchInput,
  UnsavedChangesBar,
  useUnsavedGuard,
  useConfirm,
  type DesignTone,
} from "../../ui";

// ── Workflow Inbox ─────────────────────────────────────────────────────────────

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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
      qc.invalidateQueries({ queryKey: ["wf-history"] });
    },
  });

  const [comment, setComment] = useState<Record<string, string>>({});

  if (isLoading) return <LoadingState label="Loading tasks…" />;

  // No Start section here — separation of duties (owner-decided,
  // 2026-08-30, reversing 08-29): business USERS submit approval requests,
  // business ADMINS decide them. The server enforces the same rule.
  return (
    <div className="mvx-admin-stack">
      {tasks.length === 0 && <EmptyState label="No pending approval tasks." />}
      {tasks.map((t: Task) => (
        <div key={t.id} className="mvx-admin-object">
          <div className="mvx-admin-object__header">
            <div className="mvx-admin-object__title">
              <div className="mvx-admin-object__name">{t.workflow_name}</div>
              <div className="mvx-admin-object__meta">
                Step: <strong>{t.step_name || t.step_def_id}</strong>
                {t.context?.revision && <span> · {t.context.revision} / {t.context.version}</span>}
              </div>
            </div>
            <StatusBadge tone="warning">Pending</StatusBadge>
          </div>

          <div className="mvx-admin-object__body">
            {t.rework_count ? (
              <p className="mvx-admin-muted" style={{ fontSize: 13, margin: 0 }}>{t.rework_note}</p>
            ) : null}
            {t.instructions && (
              <p style={{ fontSize: 13, margin: 0, lineHeight: 1.5 }}>{t.instructions}</p>
            )}
            {/* What is being approved: the instance's scope with names, not
                codes — "country · geography: Canada (CA)" — an approver
                deciding on a bare code is still deciding half-blind. */}
            <TaskContextSummary contextDisplay={t.context_display} context={t.context} />
            <div className="mvx-admin-muted">
              {t.requested_by && <span>Requested by <strong>{t.requested_by}</strong> · </span>}
              Instance: {t.instance_id.slice(0, 8)} · {new Date(t.created_at).toLocaleString()}
            </div>

            <TextInput
              placeholder={t.required_comment ? "Add a comment (required)" : "Add a comment (optional)"}
              value={comment[t.id] ?? ""}
              onChange={(e) => setComment((c) => ({ ...c, [t.id]: e.target.value }))}
            />

            <WorkflowStepActions
              stepType={t.step_type}
              completionLabel={t.completion_label}
              condition={t.condition}
              disabled={complete.isPending || (t.required_comment && !comment[t.id]?.trim())}
              onDecide={decision => complete.mutate({ stepId: t.id, decision, comment: comment[t.id] ?? "" })}
            />

            {complete.isError && (
              <p className="mvx-admin-error">{(complete.error as Error).message}</p>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

// ── History ────────────────────────────────────────────────────────────────────

const WF_STATUS_TONE: Record<string, DesignTone> = {
  completed: "success",
  running: "warning",
  cancelled: "neutral",
  failed: "danger",
};

const DECISION_TONE: Record<string, DesignTone> = {
  approve: "success",
  reject: "danger",
  sent: "brand",
};

export function WorkflowHistory({ focusInstanceId }: { focusInstanceId?: string } = {}) {
  const qc = useQueryClient();
  const { data: instances = [], isLoading } = useQuery({
    queryKey: ["wf-history"],
    queryFn: api.getWorkflowHistory,
    refetchInterval: 10_000,
  });

  useEffect(() => {
    if (!focusInstanceId) return;
    document.getElementById(`wf-history-${focusInstanceId}`)?.scrollIntoView({ behavior: "smooth", block: "center" });
  }, [focusInstanceId, instances]);

  const [statusFilter, setStatusFilter] = useState<string>("all");
  const [nameFilter, setNameFilter] = useState<string>("");
  const [stepComments, setStepComments] = useState<Record<string, string>>({});
  const [instanceStatus, setInstanceStatus] = useState<Record<string, string>>({});

  const complete = useMutation({
    mutationFn: ({ stepId, decision, comment }: { stepId: string; decision: string; comment: string }) =>
      api.completeTask(stepId, decision, comment),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["wf-history"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
    },
  });

  const changeStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: string }) =>
      api.updateWorkflowInstance(id, status),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["wf-history"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
    },
  });

  const filtered = useMemo(() => {
    return instances.filter((inst: HistoryInstance) => {
      if (statusFilter !== "all" && inst.status !== statusFilter) return false;
      if (nameFilter.trim() && !inst.workflow_name.toLowerCase().includes(nameFilter.trim().toLowerCase())) return false;
      return true;
    });
  }, [instances, statusFilter, nameFilter]);

  if (isLoading) return <LoadingState label="Loading history…" />;

  return (
    <div className="mvx-admin-stack">
      <Toolbar>
        <ToolbarGroup>
          <SearchInput
            placeholder="Search by workflow name…"
            value={nameFilter}
            onChange={e => setNameFilter(e.target.value)}
            width={240}
          />
          <Select value={statusFilter} onChange={e => setStatusFilter(e.target.value)} style={{ minWidth: 140 }}>
            <option value="all">All statuses</option>
            <option value="running">Running</option>
            <option value="completed">Completed</option>
            <option value="cancelled">Cancelled</option>
            <option value="failed">Failed</option>
          </Select>
          <span className="mvx-admin-muted">{filtered.length} of {instances.length}</span>
        </ToolbarGroup>
      </Toolbar>

      {filtered.length === 0 && (
        <EmptyState label="No workflow history matching the current filter." />
      )}

      {filtered.map((inst: HistoryInstance) => {
        const pendingSteps = (inst.steps ?? []).filter(s => s.status === "in_progress");
        return (
          <div
            key={inst.id}
            id={`wf-history-${inst.id}`}
            className="mvx-admin-object"
            style={inst.id === focusInstanceId ? { boxShadow: "0 0 0 2px var(--color-brand-600)" } : undefined}
          >
            <div className="mvx-admin-object__header">
              <div className="mvx-admin-object__title">
                <span className="mvx-admin-object__name">{inst.workflow_name}</span>
                {inst.context?.revision && (
                  <span className="mvx-admin-muted" style={{ marginLeft: 8 }}>
                    {inst.context.revision} / {inst.context.version}
                  </span>
                )}
              </div>
              <span className="mvx-admin-muted" style={{ whiteSpace: "nowrap" }}>{new Date(inst.started_at).toLocaleDateString()}</span>
              <StatusBadge tone={WF_STATUS_TONE[inst.status] ?? "neutral"}>{inst.status}</StatusBadge>
              <Select
                value={instanceStatus[inst.id] ?? inst.status}
                onChange={e => setInstanceStatus(m => ({ ...m, [inst.id]: e.target.value }))}
                aria-label="Change instance status"
                // Bounded: the shared .mvx-select is width:100%, which in this
                // flex header would grab every free pixel and crush the title.
                style={{ width: 150, flex: "0 0 auto" }}
              >
                <option value="running">Running</option>
                <option value="completed">Completed</option>
                <option value="cancelled">Cancelled</option>
                <option value="failed">Failed</option>
              </Select>
              <Button
                size="sm"
                disabled={changeStatus.isPending || (instanceStatus[inst.id] ?? inst.status) === inst.status}
                onClick={() => changeStatus.mutate({ id: inst.id, status: instanceStatus[inst.id] ?? inst.status })}
              >
                Apply
              </Button>
            </div>

            {/* Steps */}
            <div className="mvx-admin-object__body" style={{ gap: 6 }}>
              <TaskContextSummary contextDisplay={inst.context_display} context={inst.context} />
              {(inst.steps ?? []).map((s, i) => (
                <div key={s.id} className="mvx-wf-step">
                  <span className="mvx-wf-step__index">{i + 1}</span>
                  <span className="mvx-wf-step__name">{s.step_name || s.step_def_id}</span>
                  {s.decision && (
                    <StatusBadge tone={DECISION_TONE[s.decision] ?? "neutral"}>{s.decision}</StatusBadge>
                  )}
                  {s.comment && s.comment !== "Auto-dispatched" && (
                    <span className="mvx-wf-step__comment">"{s.comment}"</span>
                  )}
                  <StatusBadge tone={s.status === "completed" ? "success" : s.status === "in_progress" ? "warning" : "neutral"}>
                    {s.status}
                  </StatusBadge>
                </div>
              ))}
            </div>

            {/* Action area for pending steps */}
            {pendingSteps.length > 0 && (
              <div className="mvx-wf-pending">
                <div className="mvx-wf-pending__label">Pending Actions</div>
                {pendingSteps.map(s => (
                  <div key={s.id} className="mvx-wf-pending__step">
                    <div>Step: <strong>{s.step_name || s.step_def_id}</strong></div>
                    <TextInput
                      placeholder="Add a comment (optional)"
                      value={stepComments[s.id] ?? ""}
                      onChange={e => setStepComments(c => ({ ...c, [s.id]: e.target.value }))}
                    />
                    <WorkflowStepActions
                      stepType={s.step_type}
                      size="sm"
                      disabled={complete.isPending}
                      onDecide={decision => complete.mutate({ stepId: s.id, decision, comment: stepComments[s.id] ?? "" })}
                    />
                  </div>
                ))}
                {complete.isError && (
                  <p className="mvx-admin-error">{(complete.error as Error).message}</p>
                )}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// ── Roles Tab ──────────────────────────────────────────────────────────────────

export function RolesTab() {
  const qc = useQueryClient();
  const [newName, setNewName] = useState("");
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [addingUser, setAddingUser] = useState<string | null>(null);
  const [selectedUserId, setSelectedUserId] = useState<string>("");

  const { data: roles = [] } = useQuery<BARole[]>({ queryKey: ["ba-roles"], queryFn: api.listBARoles });
  const { data: allUsers = [] } = useQuery<BAUser[]>({ queryKey: ["ba-users"], queryFn: api.listBAUsers });
  const { data: allDashboards = [] } = useQuery<BAAvailableItem[]>({
    queryKey: ["ba-available-dashboards"],
    queryFn: () => api.getBAAvailable("dashboards"),
  });

  const createRole = useMutation({
    mutationFn: (name: string) => api.createBARole(name),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["ba-roles"] }); setNewName(""); },
  });
  const updateRole = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => api.updateBARole(id, name),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["ba-roles"] }); setEditingId(null); },
  });
  const deleteRole = useMutation({
    mutationFn: (id: string) => api.deleteBARole(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ba-roles"] }),
  });
  const setDashboards = useMutation({
    mutationFn: ({ id, ids }: { id: string; ids: string[] }) => api.setRoleDashboards(id, ids),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ba-roles"] }),
  });
  const addMember = useMutation({
    mutationFn: ({ roleId, userId }: { roleId: string; userId: string }) => api.addRoleMember(roleId, userId),
    onSuccess: (_, { roleId }) => {
      qc.invalidateQueries({ queryKey: ["ba-roles"] });
      qc.invalidateQueries({ queryKey: ["ba-role-members", roleId] });
      setAddingUser(null);
    },
  });
  const removeMember = useMutation({
    mutationFn: ({ roleId, userId }: { roleId: string; userId: string }) => api.removeRoleMember(roleId, userId),
    onSuccess: (_, { roleId }) => {
      qc.invalidateQueries({ queryKey: ["ba-roles"] });
      qc.invalidateQueries({ queryKey: ["ba-role-members", roleId] });
    },
  });
  const [dashboardDraft, setDashboardDraft] = useState<Record<string, string[]>>({});
  const dashboardsDirty = Object.keys(dashboardDraft).length > 0;
  const { guard: guardDashboards, guardElement: dashboardGuardElement } = useUnsavedGuard(dashboardsDirty);
  const { confirm, confirmElement } = useConfirm();

  async function saveDashboardAssignments() {
    for (const [roleID, ids] of Object.entries(dashboardDraft)) {
      await setDashboards.mutateAsync({ id: roleID, ids });
    }
    setDashboardDraft({});
  }

  // Dashboard assignment is edited as a draft and applied by one Save, the
  // same way the access-rules panel in this console already works. Every
  // checkbox used to PUT the whole list immediately, so a mis-click was live
  // for every member of the role before it could be undone, and a
  // half-finished set of changes was indistinguishable from a finished one.
  // Keyed by role id; absent means "no pending edits for that role".
  function assignedDashboards(role: BARole): string[] {
    return dashboardDraft[role.id] ?? role.dashboard_ids;
  }

  function toggleDashboard(role: BARole, dashId: string) {
    const current = assignedDashboards(role);
    const next = current.includes(dashId)
      ? current.filter(d => d !== dashId)
      : [...current, dashId];
    setDashboardDraft(prev => ({ ...prev, [role.id]: next }));
  }

  return (
    <div className="mvx-admin-stack">
      {dashboardGuardElement}
      {dashboardsDirty && (
        <UnsavedChangesBar
          onSave={() => { void saveDashboardAssignments(); }}
          onCancel={() => setDashboardDraft({})}
          saveLabel="Save dashboard access"
          saving={setDashboards.isPending}
          error={setDashboards.isError ? (setDashboards.error as Error).message : undefined}
        />
      )}
      {/* Create */}
      <Toolbar>
        <ToolbarGroup>
          <TextInput
            value={newName} onChange={e => setNewName(e.target.value)}
            onKeyDown={e => e.key === "Enter" && newName.trim() && createRole.mutate(newName.trim())}
            placeholder="New role name…"
            style={{ width: 220 }}
          />
          <Button
            variant="primary"
            leadingIcon={<Plus size={14} />}
            disabled={!newName.trim()}
            loading={createRole.isPending}
            loadingLabel="Adding…"
            onClick={() => createRole.mutate(newName.trim())}
          >
            Add role
          </Button>
        </ToolbarGroup>
      </Toolbar>
      {createRole.isError && (
        <p className="mvx-admin-error">{(createRole.error as Error).message}</p>
      )}

      {roles.length === 0 && (
        <EmptyState label="No roles yet. Create one above." />
      )}

      {roles.map(role => {
        const expanded = expandedId === role.id;
        return (
          <div key={role.id} className="mvx-admin-object">
            {/* Header */}
            <div className="mvx-admin-object__header" style={expanded ? undefined : { borderBottom: "none" }}>
              {editingId === role.id ? (
                <div className="mvx-admin-inline-form" style={{ flex: 1 }}>
                  <TextInput value={editName} onChange={e => setEditName(e.target.value)}
                    onKeyDown={e => e.key === "Enter" && updateRole.mutate({ id: role.id, name: editName })}
                    style={{ maxWidth: 240 }} autoFocus />
                  <Button variant="primary" size="sm" onClick={() => updateRole.mutate({ id: role.id, name: editName })}>Save</Button>
                  <Button size="sm" onClick={() => setEditingId(null)}>Cancel</Button>
                </div>
              ) : (
                <>
                  <div className="mvx-admin-object__title">
                    <span className="mvx-admin-object__name">{role.name}</span>
                    <div className="mvx-admin-object__meta">{role.member_count} user{role.member_count !== 1 ? "s" : ""}</div>
                  </div>
                  <Button size="sm" variant="ghost" leadingIcon={<Pencil size={13} />} onClick={() => { setEditingId(role.id); setEditName(role.name); }}>
                    Rename
                  </Button>
                  <Button
                    size="sm"
                    onClick={() => guardDashboards(() => {
                      setExpandedId(expanded ? null : role.id);
                      setSelectedUserId("");
                      setAddingUser(null);
                      setDashboardDraft({});
                    })}
                  >
                    {expanded ? "Collapse" : "Configure"}
                  </Button>
                  <IconButton
                    aria-label={`Delete role ${role.name}`}
                    title="Delete role"
                    danger
                    onClick={() => confirm({ title: "Delete role?", body: `This removes "${role.name}" and unassigns all its members and dashboards.`, confirmLabel: "Delete role", onConfirm: () => deleteRole.mutate(role.id) })}
                  >
                    <Trash2 size={14} />
                  </IconButton>
                </>
              )}
            </div>

            {expanded && (
              <div className="mvx-admin-object__body" style={{ flexDirection: "row", gap: 32 }}>
                {/* Dashboards */}
                <div style={{ flex: 1 }}>
                  <div className="mvx-admin-revisions__label" style={{ marginBottom: 10 }}>
                    Visible Dashboards
                  </div>
                  {allDashboards.length === 0 && <p className="mvx-admin-muted">No dashboards yet</p>}
                  {(() => {
                    const hasGroups = allDashboards.some(d => d.group);
                    if (!hasGroups) {
                      return allDashboards.map(d => (
                        <Checkbox
                          key={d.id}
                          label={d.name}
                          checked={assignedDashboards(role).includes(d.id)}
                          onChange={() => toggleDashboard(role, d.id)}
                        />
                      ));
                    }
                    // Group by category
                    const groups = new Map<string, BAAvailableItem[]>();
                    allDashboards.forEach(d => {
                      const key = d.group ?? "";
                      if (!groups.has(key)) groups.set(key, []);
                      groups.get(key)!.push(d);
                    });
                    const sortedKeys = [...groups.keys()].sort((a, b) => {
                      if (a === "") return 1;
                      if (b === "") return -1;
                      return a.localeCompare(b);
                    });
                    return sortedKeys.map(cat => (
                      <div key={cat} style={{ marginBottom: 12 }}>
                        {cat && (
                          <div className="mvx-admin-revisions__label" style={{ marginBottom: 6 }}>
                            {cat}
                          </div>
                        )}
                        <div style={{ paddingLeft: cat ? 8 : 0 }}>
                          {(groups.get(cat) ?? []).map(d => (
                            <Checkbox
                              key={d.id}
                              label={d.name}
                              checked={assignedDashboards(role).includes(d.id)}
                              onChange={() => toggleDashboard(role, d.id)}
                            />
                          ))}
                        </div>
                      </div>
                    ));
                  })()}
                </div>

                {/* Members */}
                <div style={{ flex: 1 }}>
                  <div className="mvx-admin-revisions__label" style={{ marginBottom: 10 }}>
                    Members
                  </div>
                  <MemberList role={role} onRemove={userId => removeMember.mutate({ roleId: role.id, userId })} />
                  {addingUser === role.id ? (
                    <div className="mvx-admin-inline-form" style={{ marginTop: 10 }}>
                      <Select value={selectedUserId} onChange={e => setSelectedUserId(e.target.value)} style={{ flex: 1 }}>
                        <option value="">— select user —</option>
                        {allUsers.map(u => (
                          <option key={u.id} value={u.id}>{u.display_name}</option>
                        ))}
                      </Select>
                      <Button variant="primary" size="sm" disabled={!selectedUserId}
                        onClick={() => { if (selectedUserId) addMember.mutate({ roleId: role.id, userId: selectedUserId }); }}>
                        Add
                      </Button>
                      <IconButton aria-label="Cancel adding member" size={28} onClick={() => { setAddingUser(null); setSelectedUserId(""); }}>
                        <X size={14} />
                      </IconButton>
                    </div>
                  ) : (
                    <Button size="sm" variant="ghost" leadingIcon={<Plus size={13} />} style={{ marginTop: 8 }} onClick={() => { setAddingUser(role.id); setSelectedUserId(""); }}>
                      Add member
                    </Button>
                  )}
                </div>
              </div>
            )}
          </div>
        );
      })}
      {confirmElement}
    </div>
  );
}

function MemberList({ role, onRemove }: { role: BARole; onRemove: (id: string) => void }) {
  const { data: members = [] } = useQuery({
    queryKey: ["ba-role-members", role.id],
    queryFn: () => api.listRoleMembers(role.id),
  });
  if (members.length === 0) return <p className="mvx-admin-muted">No members yet</p>;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {members.map(m => (
        <div key={m.user_id} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13 }}>
          <span style={{ flex: 1 }}>{m.display_name}</span>
          <span className="mvx-admin-muted">{m.email}</span>
          <IconButton aria-label={`Remove ${m.display_name} from role`} danger size={26} onClick={() => onRemove(m.user_id)}>
            <X size={13} />
          </IconButton>
        </div>
      ))}
    </div>
  );
}

// ── Access Rules Tab ────────────────────────────────────────────────────────────

type AccessLevel = "write" | "read" | "hidden";

const ACCESS_COLOR: Record<AccessLevel, string> = {
  write: "var(--color-success)",
  read: "var(--color-info)",
  hidden: "var(--color-text-muted)",
};

export function AccessRulesTab() {
  const qc = useQueryClient();

  const { data: users = [] } = useQuery<BAUser[]>({ queryKey: ["ba-users"], queryFn: api.listBAUsers });
  const { data: dimMembers = [] } = useQuery<BAAvailableItem[]>({
    queryKey: ["ba-available-dimension-members"],
    queryFn: () => api.getBAAvailable("dimension_members"),
  });
  const { data: metrics = [] } = useQuery<BAAvailableItem[]>({
    queryKey: ["ba-available-metrics"],
    queryFn: () => api.getBAAvailable("metrics"),
  });

  const dimGroups = useMemo(() => {
    const groups: Record<string, BAAvailableItem[]> = {};
    dimMembers.forEach(m => {
      const g = m.group ?? "Dimension";
      if (!groups[g]) groups[g] = [];
      groups[g].push(m);
    });
    return groups;
  }, [dimMembers]);

  const [selectedUser, setSelectedUser] = useState<string>("");
  const [draft, setDraft] = useState<Record<string, AccessLevel>>({});
  const [dirty, setDirty] = useState(false);
  // Switching user used to reset the draft outright, so unsaved rule edits
  // disappeared with no prompt; the same edits were also lost on reload or
  // Back, since nothing registered a beforeunload handler.
  const { guard, guardElement } = useUnsavedGuard(dirty);

  const { data: existingRules = [] } = useQuery<UserAccessRule[]>({
    queryKey: ["ba-access-rules", selectedUser],
    queryFn: () => api.getUserAccessRules(selectedUser),
    enabled: !!selectedUser,
  });

  // Clear draft immediately when user changes
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- intentional reset when selection changes
    setDraft({});
    setDirty(false);
  }, [selectedUser]);

  // Populate draft once query data arrives for the selected user
  useEffect(() => {
    if (!selectedUser) return;
    const map: Record<string, AccessLevel> = {};
    existingRules.forEach(r => { map[`${r.rule_type}:${r.ref_id}`] = r.access as AccessLevel; });
    // eslint-disable-next-line react-hooks/set-state-in-effect -- initialize draft from server data
    setDraft(map);
    setDirty(false);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [existingRules]); // intentionally omit selectedUser — only fire when data changes

  const saveRules = useMutation({
    mutationFn: (rules: UserAccessRule[]) => api.putUserAccessRules(selectedUser, rules),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["ba-access-rules", selectedUser] });
      setDirty(false);
    },
  });

  function getAccess(type: string, id: string): AccessLevel {
    return (draft[`${type}:${id}`] ?? "write") as AccessLevel;
  }

  function setAccess(type: string, id: string, level: AccessLevel) {
    setDraft(d => ({ ...d, [`${type}:${id}`]: level }));
    setDirty(true);
  }

  function resetDraft() {
    const map: Record<string, AccessLevel> = {};
    existingRules.forEach(r => { map[`${r.rule_type}:${r.ref_id}`] = r.access as AccessLevel; });
    setDraft(map);
    setDirty(false);
  }

  function handleSave() {
    // Only persist non-default (non-write) rules; PUT replaces the full set
    const rules: UserAccessRule[] = Object.entries(draft)
      .filter(([, access]) => access !== "write")
      .map(([key, access]) => {
        const sep = key.indexOf(":");
        return {
          rule_type: key.slice(0, sep) as UserAccessRule["rule_type"],
          ref_id: key.slice(sep + 1),
          ref_name: "",
          access,
        };
      });
    saveRules.mutate(rules);
  }

  function accessSelect(type: string, id: string) {
    const val = getAccess(type, id);
    return (
      <Select
        value={val}
        onChange={e => setAccess(type, id, e.target.value as AccessLevel)}
        style={{ color: ACCESS_COLOR[val] }}
        aria-label="Access level"
      >
        <option value="write">Write</option>
        <option value="read">Read</option>
        <option value="hidden">Hidden</option>
      </Select>
    );
  }

  return (
    <div className="mvx-admin-stack">
      {guardElement}
      {dirty && (
        <UnsavedChangesBar
          onSave={handleSave}
          onCancel={resetDraft}
          saveLabel="Save rules"
          saving={saveRules.isPending}
        />
      )}

      <Toolbar>
        <ToolbarGroup>
          <span style={{ fontSize: 13, fontWeight: 500 }}>User:</span>
          <Select
            value={selectedUser}
            onChange={e => { const next = e.target.value; guard(() => setSelectedUser(next)); }}
            style={{ minWidth: 220 }}
            aria-label="Select user"
          >
            <option value="">— select a user —</option>
            {users.map(u => (
              <option key={u.id} value={u.id}>{u.display_name}</option>
            ))}
          </Select>
          {saveRules.isSuccess && !dirty && (
            <span style={{ fontSize: 13, color: "var(--color-success)", display: "inline-flex", alignItems: "center", gap: 4 }}>
              <CheckCircle2 size={14} /> Saved
            </span>
          )}
        </ToolbarGroup>
      </Toolbar>

      {!selectedUser && (
        <EmptyState label="Select a user to configure their access rules." />
      )}

      {selectedUser && (
        <div className="mvx-table-wrap">
          <table className="mvx-table mvx-table--compact">
            <thead>
              <tr>
                <th>Resource</th>
                <th>Type</th>
                <th>Access</th>
              </tr>
            </thead>
            <tbody>
              {Object.entries(dimGroups).map(([groupName, members]) => (
                <React.Fragment key={groupName}>
                  <tr>
                    <td colSpan={3} className="mvx-table__group-row">{groupName}</td>
                  </tr>
                  {members.map(m => (
                    <tr key={m.id}>
                      <td style={{ paddingLeft: 24 }}>{m.name}</td>
                      <td className="mvx-admin-muted">dimension member</td>
                      <td>{accessSelect("dimension_member", m.id)}</td>
                    </tr>
                  ))}
                </React.Fragment>
              ))}
              {metrics.length > 0 && (
                <tr>
                  <td colSpan={3} className="mvx-table__group-row">Metrics</td>
                </tr>
              )}
              {metrics.map(m => (
                <tr key={m.id}>
                  <td>{m.name}</td>
                  <td className="mvx-admin-muted">metric</td>
                  <td>{accessSelect("metric", m.id)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// ── Main Console ───────────────────────────────────────────────────────────────
