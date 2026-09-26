import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { Button, SearchInput, Select as UiSelect, SectionHeader, LoadingState, EmptyState } from "../../ui";
import { api } from "../../api/client";
import type { WorkflowStepDef, WorkflowDefSummary, TriggerEventCatalogItem } from "../../api/client";
import { TriggerEventSelect, StatusBadge, Modal, Field } from "./WorkflowShared";
import { TRIGGER_FALLBACK, useTriggerEvents, inputStyle, btnPrimary, btnSecondary } from "./workflowConstants";
import { CreateAutomationModal } from "./CreateAutomationModal";
import { WorkflowEditor } from "./WorkflowEditor";

const WORKFLOW_TEMPLATES: { name: string; description: string; steps: WorkflowStepDef[] }[] = [
  {
    name: "Simple Approval",
    description: "Single approval step with notification",
    steps: [
      { id: "step-approval-1", name: "Manager Approval", type: "approval", instructions: "Review and approve or reject.", assignee_roles: [], sla_hours: 48, routes: { approve: "step-notify-1", reject: "end-rejected" } },
      { id: "step-notify-1", name: "Notify Requester", type: "notification", notification: { recipient_type: "requester", subject: "Your request was approved", message: "Your request has been approved." }, routes: { next: "end-completed" } },
    ],
  },
  {
    name: "Two-Level Approval",
    description: "Manager then Finance approval",
    steps: [
      { id: "step-mgr", name: "Manager Approval", type: "approval", assignee_roles: [], sla_hours: 48, routes: { approve: "step-finance", reject: "end-rejected" } },
      { id: "step-finance", name: "Finance Approval", type: "approval", assignee_roles: [], sla_hours: 72, routes: { approve: "step-notify", reject: "end-rejected" } },
      { id: "step-notify", name: "Notify Requester", type: "notification", notification: { recipient_type: "requester", subject: "Approved", message: "Your request has been fully approved." }, routes: { next: "end-completed" } },
    ],
  },
  {
    name: "Amount-Based Approval",
    description: "Route to Finance only if amount exceeds threshold",
    steps: [
      { id: "step-condition", name: "Amount Threshold", type: "condition", condition: { left: "amount", operator: "greater_than", right: 10000 }, routes: { true: "step-finance", false: "step-notify" } },
      { id: "step-finance", name: "Finance Approval", type: "approval", assignee_roles: [], sla_hours: 72, routes: { approve: "step-notify", reject: "end-rejected" } },
      { id: "step-notify", name: "Notify Requester", type: "notification", notification: { recipient_type: "requester", subject: "Approved", message: "Your request is approved." }, routes: { next: "end-completed" } },
    ],
  },
  {
    name: "Notify Only",
    description: "Send a notification and complete",
    steps: [
      { id: "step-notify-1", name: "Send Notification", type: "notification", notification: { recipient_type: "role", subject: "Action Required", message: "Please review the submitted data." }, routes: { next: "end-completed" } },
    ],
  },
];

// ── WorkflowList ─────────────────────────────────────────────────────────────

interface ListProps {
  applicationId: string;
  revisionId?: string;
  onOpen: (id: string) => void;
}

function WorkflowList({ applicationId, revisionId, onOpen }: ListProps) {
  const qc = useQueryClient();
  const [search, setSearch] = useState("");
  const [filterStatus, setFilterStatus] = useState<string>("all");
  const [showCreate, setShowCreate] = useState(false);
  const [showTemplates, setShowTemplates] = useState(false);
  const [newName, setNewName] = useState("");
  const [newDesc, setNewDesc] = useState("");
  const [newTrigger, setNewTrigger] = useState("manual");
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  const { data: workflows = [], isLoading } = useQuery({
    queryKey: ["dev-workflows", applicationId, revisionId],
    queryFn: () => api.listWorkflowDefs(applicationId, revisionId),
    enabled: !!applicationId,
  });

  const { data: triggerEvents = TRIGGER_FALLBACK } = useTriggerEvents(applicationId);

  const createMutation = useMutation({
    mutationFn: (body: { name: string; description: string; trigger_event: string }) =>
      api.createWorkflowDef(applicationId, body, revisionId),
    onSuccess: (def) => {
      qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] });
      setShowCreate(false);
      setNewName("");
      setNewDesc("");
      onOpen(def.id);
    },
  });

  const fromTemplateMutation = useMutation({
    mutationFn: async (tpl: typeof WORKFLOW_TEMPLATES[0]) => {
      const def = await api.createWorkflowDef(applicationId, {
        name: tpl.name, description: tpl.description, trigger_event: "manual",
      }, revisionId);
      return api.updateWorkflowDef(def.id, { steps: tpl.steps });
    },
    onSuccess: (def) => {
      qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] });
      setShowTemplates(false);
      onOpen(def.id);
    },
  });

  const archiveMutation = useMutation({
    mutationFn: (id: string) => api.archiveWorkflowDef(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] }),
  });
  const restoreMutation = useMutation({
    mutationFn: (id: string) => api.restoreWorkflowDef(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] }),
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => api.deleteWorkflowDef(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] });
      setConfirmDelete(null);
    },
  });

  const dupMutation = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => api.duplicateWorkflowDef(id, name),
    onSuccess: (def) => {
      qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] });
      onOpen(def.id);
    },
  });

  const filtered = workflows.filter(w => {
    const matchStatus = filterStatus === "all" || w.status === filterStatus;
    const matchSearch = w.name.toLowerCase().includes(search.toLowerCase());
    return matchStatus && matchSearch;
  });

  return (
    <div style={{ padding: "24px 28px", maxWidth: 1100 }}>
      {/* Header */}
      <SectionHeader
        title="Workflows"
        subtitle="Define reusable approval and task processes."
        actions={
          <>
            <Button onClick={() => setShowTemplates(true)}>From Template</Button>
            <Button variant="primary" leadingIcon={<Plus size={14} />} onClick={() => setShowCreate(true)}>
              New Workflow
            </Button>
          </>
        }
      />

      {/* Filters */}
      <div style={{ display: "flex", gap: 10, marginBottom: 16 }}>
        <SearchInput
          value={search}
          onChange={e => setSearch(e.target.value)}
          placeholder="Search by name…"
          width={300}
        />
        <UiSelect
          value={filterStatus}
          onChange={e => setFilterStatus(e.target.value)}
          aria-label="Filter by status"
        >
          <option value="all">All statuses</option>
          <option value="draft">Draft</option>
          <option value="published">Published</option>
          <option value="archived">Archived</option>
          <option value="invalid">Invalid</option>
        </UiSelect>
      </div>

      {/* Table */}
      {isLoading ? (
        <LoadingState />
      ) : filtered.length === 0 ? (
        <EmptyState
          label={workflows.length === 0
            ? "No workflows yet. Create a workflow to define approvals, tasks, conditions, and notifications."
            : "No workflows match. Try a different filter or search."}
        />
      ) : (
        <div className="mvx-table-wrap">
        <table className="mvx-table">
          <thead>
            <tr>
              {["Name", "Status", "Trigger", "Steps", "Updated", ""].map(h => (
                <th key={h}>{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {filtered.map(w => (
              <WorkflowRow
                key={w.id}
                workflow={w}
                onOpen={() => onOpen(w.id)}
                onDuplicate={() => dupMutation.mutate({ id: w.id, name: `${w.name} (copy)` })}
                onArchive={() => archiveMutation.mutate(w.id)}
                onRestore={() => restoreMutation.mutate(w.id)}
                onDelete={() => setConfirmDelete(w.id)}
                triggerEvents={triggerEvents}
              />
            ))}
          </tbody>
        </table>
        </div>
      )}

      {/* Create modal */}
      {showCreate && (
        <Modal title="New Workflow" onClose={() => setShowCreate(false)}>
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            <Field label="Name">
              <input
                autoFocus
                value={newName}
                onChange={e => setNewName(e.target.value)}
                placeholder="e.g. Budget Approval"
                style={inputStyle}
              />
            </Field>
            <Field label="Description">
              <input
                value={newDesc}
                onChange={e => setNewDesc(e.target.value)}
                placeholder="Optional"
                style={inputStyle}
              />
            </Field>
            <Field label="Trigger event">
              <TriggerEventSelect
                applicationId={applicationId}
                value={newTrigger}
                onChange={setNewTrigger}
              />
            </Field>
            {createMutation.isError && (
              <div style={{ color: "var(--color-danger)", fontSize: 13 }}>{String(createMutation.error)}</div>
            )}
            <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
              <button onClick={() => setShowCreate(false)} style={btnSecondary}>Cancel</button>
              <button
                onClick={() => createMutation.mutate({ name: newName, description: newDesc, trigger_event: newTrigger })}
                disabled={!newName.trim() || createMutation.isPending}
                style={btnPrimary}>
                Create
              </button>
            </div>
          </div>
        </Modal>
      )}

      {/* Templates modal */}
      {showTemplates && (
        <Modal title="Choose a Template" onClose={() => setShowTemplates(false)}>
          <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
            {WORKFLOW_TEMPLATES.map(tpl => (
              <button
                key={tpl.name}
                onClick={() => fromTemplateMutation.mutate(tpl)}
                disabled={fromTemplateMutation.isPending}
                style={{ textAlign: "left", padding: "12px 14px", border: "1px solid var(--color-border)", borderRadius: 8, background: "var(--color-surface)", cursor: "pointer" }}>
                <div style={{ fontWeight: 600, fontSize: 14, color: "var(--color-text)" }}>{tpl.name}</div>
                <div style={{ fontSize: 12, color: "var(--color-text-quiet)", marginTop: 2 }}>{tpl.description}</div>
              </button>
            ))}
            <button onClick={() => setShowTemplates(false)} style={{ ...btnSecondary, marginTop: 4 }}>Cancel</button>
          </div>
        </Modal>
      )}

      {/* Delete confirmation */}
      {confirmDelete && (
        <Modal title="Delete Workflow" onClose={() => setConfirmDelete(null)}>
          <p style={{ fontSize: 14, color: "var(--color-text-strong)" }}>
            Delete this draft workflow? This cannot be undone.
          </p>
          {deleteMutation.isError && (
            <div style={{ color: "var(--color-danger)", fontSize: 13, marginBottom: 12 }}>{String(deleteMutation.error)}</div>
          )}
          <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
            <button onClick={() => setConfirmDelete(null)} style={btnSecondary}>Cancel</button>
            <button
              onClick={() => deleteMutation.mutate(confirmDelete!)}
              disabled={deleteMutation.isPending}
              style={{ ...btnPrimary, background: "var(--color-danger-solid)" }}>
              Delete
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}

function WorkflowRow({ workflow, onOpen, onDuplicate, onArchive, onRestore, onDelete, triggerEvents }: {
  workflow: WorkflowDefSummary;
  onOpen: () => void;
  onDuplicate: () => void;
  onArchive: () => void;
  onRestore: () => void;
  onDelete: () => void;
  triggerEvents: TriggerEventCatalogItem[];
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  // The actions menu positions itself with fixed coordinates from the
  // trigger button: absolutely-positioned inside the row it gets clipped by
  // the table panel's overflow, which on the last row left a one-item
  // sliver of menu poking out of the panel edge (reported live as "3 dots
  // shows UI error").
  const [menuPos, setMenuPos] = useState<{ top: number; right: number } | null>(null);
  const [showCreateAutomation, setShowCreateAutomation] = useState(false);
  const updatedAt = new Date(workflow.updated_at).toLocaleDateString();
  const triggerLabel = triggerEvents.find(e => e.key === workflow.trigger_event)?.label ?? workflow.trigger_event;

  return (
    <>
      <tr style={{ borderBottom: "1px solid var(--color-surface-muted)" }} onMouseLeave={() => setMenuOpen(false)}>
        <td style={{ padding: "10px 12px" }}>
          <button
            onClick={onOpen}
            style={{ background: "none", border: "none", padding: 0, cursor: "pointer", fontWeight: 600, color: "var(--color-text)", fontSize: 13 }}>
            {workflow.name}
          </button>
          {workflow.description && (
            <div style={{ fontSize: 12, color: "var(--color-disabled)", marginTop: 2 }}>{workflow.description}</div>
          )}
        </td>
        <td style={{ padding: "10px 12px" }}><StatusBadge status={workflow.status} /></td>
        <td style={{ padding: "10px 12px", color: "var(--color-text-quiet)" }}>{triggerLabel}</td>
        <td style={{ padding: "10px 12px", color: "var(--color-text-quiet)" }}>{workflow.step_count}</td>
        <td style={{ padding: "10px 12px", color: "var(--color-disabled)" }}>{updatedAt}</td>
        <td style={{ padding: "10px 12px", position: "relative" }}>
          <button
            onClick={e => {
              const rect = e.currentTarget.getBoundingClientRect();
              setMenuPos({ top: rect.bottom + 4, right: window.innerWidth - rect.right });
              setMenuOpen(v => !v);
            }}
            style={{ background: "none", border: "1px solid var(--color-border)", borderRadius: 4, padding: "3px 8px", cursor: "pointer", fontSize: 13 }}
            title="Actions">
            ···
          </button>
          {menuOpen && menuPos && (
            <div style={{ position: "fixed", right: menuPos.right, top: menuPos.top, background: "var(--color-surface)", border: "1px solid var(--color-border)", borderRadius: 8, boxShadow: "0 4px 12px rgba(0,0,0,0.1)", zIndex: 1100, minWidth: 180 }}>
              {[
                { label: "Open", action: onOpen },
                { label: "Duplicate", action: onDuplicate },
                { label: "Create Trigger", action: () => setShowCreateAutomation(true) },
                ...(workflow.status !== "archived" ? [{ label: "Archive", action: onArchive }] : [{ label: "Restore to draft", action: onRestore }]),
                // Delete is always listed; when it is not possible it says why
                // instead of vanishing (only drafts without instances can go —
                // archive the rest).
                {
                  label: workflow.status === "draft" ? "Delete draft" : "Delete",
                  action: onDelete,
                  danger: true,
                  disabled: workflow.status !== "draft",
                  reason: workflow.status !== "draft" ? "Only a draft can be deleted — archive this one instead" : undefined,
                },
              ].map(item => (
                <button
                  key={item.label}
                  disabled={(item as { disabled?: boolean }).disabled}
                  title={(item as { reason?: string }).reason}
                  onClick={() => { item.action(); setMenuOpen(false); }}
                  style={{ display: "block", width: "100%", textAlign: "left", padding: "8px 14px", background: "none", border: "none", fontSize: 13, cursor: "pointer", color: (item as { danger?: boolean }).danger ? "var(--color-danger)" : "var(--color-text-strong)" }}>
                  {item.label}
                </button>
              ))}
            </div>
          )}
        </td>
      </tr>
      {showCreateAutomation && (
        <CreateAutomationModal workflow={workflow} onClose={() => setShowCreateAutomation(false)} />
      )}
    </>
  );
}

// ── Main Export ───────────────────────────────────────────────────────────────

export function WorkflowsTab({ revisionId }: { revisionId?: string } = {}) {
  const [editingId, setEditingId] = useState<string | null>(null);

  const { data: demoCtx } = useQuery({
    queryKey: ["demo"],
    queryFn: api.getDemo,
    staleTime: 60_000,
  });

  const applicationId = demoCtx?.app_id ?? "";

  if (!applicationId) {
    return <div style={{ padding: 32, color: "var(--color-disabled)", fontSize: 14 }}>Loading application…</div>;
  }

  if (editingId) {
    return (
      <WorkflowEditor
        defId={editingId}
        applicationId={applicationId}
        revisionId={revisionId}
        onBack={() => setEditingId(null)}
      />
    );
  }

  return (
    <WorkflowList
      applicationId={applicationId}
      revisionId={revisionId}
      onOpen={setEditingId}
    />
  );
}
