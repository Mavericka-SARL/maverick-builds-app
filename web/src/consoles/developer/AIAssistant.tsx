import React, { useState, useRef, useEffect } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Bot, Bookmark, Check, ClipboardList, Cog, FileText, History, LayoutDashboard, Link2, ListTree, Paperclip,
  Pencil, Plus, Puzzle, Rocket, Ruler, Search, Settings, Table2, Trash2, Users, Workflow, Wrench, X, Zap,
} from "lucide-react";
import { api, type AISession, type AIMessage, type AISettings, type AIProposal, type AIProposalStep, type AIProposalWithSummary, type AIDocument } from "../../api/client";
import {
  Button, IconButton, TextInput, Select, Textarea, Field, StatusBadge, RevisionBadge,
  FilterChip, InlineAlert, SectionHeader, useConfirm, type DesignTone,
} from "../../ui";

// ── Settings panel ────────────────────────────────────────────────────────────
// The model is free text, with no suggestion list: providers ship new models
// faster than a list here could follow. Blank means the gateway's default for
// the provider (providerDefaultModels in internal/gateway/ai_handler.go).

function SettingsPanel({ onClose, initialSettings }: { onClose: () => void; initialSettings?: AISettings }) {
  const qc = useQueryClient();

  // Initialize from already-loaded settings passed from parent — no sync effect needed.
  const [provider, setProvider] = useState(initialSettings?.provider ?? "openai");
  const [model, setModel]       = useState(initialSettings?.model ?? "");
  const [apiKey, setApiKey]     = useState("");
  const [saved, setSaved]       = useState(false);

  // A model name belongs to one provider, so switching provider clears it
  // (blank = that provider's default) — inline in the handler, not an effect.
  const handleProviderChange = (newProvider: string) => {
    setProvider(newProvider);
    setModel("");
  };

  const save = useMutation({
    mutationFn: () => api.aiSaveSettings({ provider, model, api_key: apiKey || undefined }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["ai-settings"] });
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    },
  });

  const test = useMutation({
    mutationFn: () => api.aiTestSettings({ provider, model, api_key: apiKey || undefined }),
  });

  return (
    <div style={{ padding: 24, maxWidth: 440 }}>
      <SectionHeader
        title="AI Developer Settings"
        actions={
          <IconButton aria-label="Close settings" title="Close" size={28} onClick={onClose}>
            <X size={14} />
          </IconButton>
        }
      />

      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        {initialSettings?.tenant_enforced ? (
          <InlineAlert tone="info">
            Your administrator supplies the AI key for this tenant ({initialSettings.tenant_provider}), and it is used for
            every call. Anything you set here is ignored.
          </InlineAlert>
        ) : initialSettings?.tenant_key ? (
          <InlineAlert tone="info">
            Your administrator supplies an AI key for this tenant ({initialSettings.tenant_provider}). You only need a key
            here if you want your calls billed to your own account instead.
          </InlineAlert>
        ) : null}

        <Field label="LLM Provider">
          <Select value={provider} onChange={e => handleProviderChange(e.target.value)} aria-label="LLM provider">
            <option value="openai">OpenAI</option>
            <option value="anthropic">Anthropic (Claude)</option>
            <option value="google">Google (Gemini)</option>
            <option value="mistral">Mistral</option>
            <option value="deepseek">DeepSeek</option>
          </Select>
        </Field>

        <Field label="Model" description="Any model ID your provider offers, including ones released after this screen was built.">
          <TextInput
            value={model}
            onChange={e => setModel(e.target.value)}
            placeholder="Leave blank for the provider's default"
            aria-label="Model"
            autoComplete="off"
          />
        </Field>

        <Field
          label={initialSettings?.has_key ? "API Key — key saved" : "API Key"}
          description={`Your prompts are sent to ${provider}. Do not include passwords or personal data.`}
        >
          <TextInput
            type="password"
            placeholder={initialSettings?.has_key ? "••••••••••••  (leave blank to keep current)" : "sk-..."}
            value={apiKey}
            onChange={e => setApiKey(e.target.value)}
            autoComplete="off"
          />
        </Field>

        <div className="mvx-admin-inline-form">
          <Button
            variant="primary"
            leadingIcon={saved ? <Check size={14} /> : undefined}
            loading={save.isPending}
            loadingLabel="Saving…"
            onClick={() => save.mutate()}
          >
            {saved ? "Saved" : "Save settings"}
          </Button>
          <Button loading={test.isPending} loadingLabel="Testing…" onClick={() => test.mutate()}>
            Test connection
          </Button>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
        </div>

        {test.data && (
          <p style={{ fontSize: 12, margin: 0, color: test.data.ok ? "var(--color-live)" : "var(--color-danger)" }}>
            {test.data.ok
              ? `✓ ${test.data.provider} / ${test.data.model} responded${test.data.reply ? `: "${test.data.reply.slice(0, 60)}"` : ""}`
              : `✗ ${test.data.error}`}
          </p>
        )}
        {test.error && <p className="mvx-admin-error">{(test.error as Error).message}</p>}
        {save.error && <p className="mvx-admin-error">{(save.error as Error).message}</p>}
      </div>
    </div>
  );
}

// ── Message bubble ────────────────────────────────────────────────────────────

function MessageBubble({ msg }: { msg: AIMessage }) {
  if (msg.role === "tool") return null; // tool results are internal

  const isUser = msg.role === "user";
  return (
    <div className={["mvx-chat-row", isUser ? "mvx-chat-row--user" : ""].filter(Boolean).join(" ")}>
      <div className={["mvx-chat-bubble", isUser ? "mvx-chat-bubble--user" : ""].filter(Boolean).join(" ")}>
        {msg.content}
        {msg.tool_calls && msg.tool_calls.length > 0 && (
          <div style={{ marginTop: 6, fontSize: 11, opacity: 0.7, display: "flex", alignItems: "center", gap: 4 }}>
            <Search size={11} aria-hidden="true" /> Fetching: {msg.tool_calls.map(tc => tc.name).join(", ")}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Proposal panel ───────────────────────────────────────────────────────────

function ToolIcon({ tool }: { tool: string }) {
  const props = { size: 14, "aria-hidden": true as const, style: { flexShrink: 0, color: "var(--color-text-muted)" } };
  switch (tool) {
    case "create_metric":        return <Ruler {...props} />;
    case "update_metric":        return <Pencil {...props} />;
    case "delete_metric":        return <Trash2 {...props} />;
    case "create_dimension":     return <ListTree {...props} />;
    case "add_dimension_member": return <Plus {...props} />;
    case "update_dimension_member": return <Pencil {...props} />;
    case "create_grid":
    case "add_grid_metric":
    case "add_grid_dimension":   return <Table2 {...props} />;
    case "create_dashboard":     return <LayoutDashboard {...props} />;
    case "add_dashboard_widget": return <Puzzle {...props} />;
    case "create_revision":      return <Bookmark {...props} />;
    case "generate_migration":   return <Cog {...props} />;
    case "apply_migration":      return <Rocket {...props} />;
    case "create_workflow_def":
    case "update_workflow_def":  return <Workflow {...props} />;
    case "delete_workflow_def":
    case "delete_form_def":
    case "delete_automation_rule":
    case "delete_form_integration": return <Trash2 {...props} />;
    case "create_form_def":
    case "update_form_def":      return <ClipboardList {...props} />;
    case "create_automation_rule":
    case "update_automation_rule": return <Zap {...props} />;
    case "create_business_role": return <Users {...props} />;
    case "create_form_integration":
    case "update_form_integration": return <Link2 {...props} />;
    default:                     return <Wrench {...props} />;
  }
}

const STEP_STATUS_TONE: Record<string, DesignTone> = { success: "success", failed: "danger" };

const TOOL_LABELS: Record<string, string> = {
  create_metric: "Create metric", update_metric: "Update metric", delete_metric: "Delete metric",
  create_dimension: "Create dimension", add_dimension_member: "Add dimension member",
  update_dimension_member: "Update dimension member",
  create_grid: "Create grid", add_grid_metric: "Add grid metric", add_grid_dimension: "Add grid dimension",
  create_dashboard: "Create dashboard", add_dashboard_widget: "Add dashboard widget",
  create_revision: "Create revision", generate_migration: "Generate migration", apply_migration: "Apply migration",
  create_workflow_def: "Create workflow", update_workflow_def: "Update workflow", delete_workflow_def: "Delete workflow",
  create_form_def: "Create form", update_form_def: "Update form", delete_form_def: "Delete form",
  create_automation_rule: "Create automation rule", update_automation_rule: "Update automation rule",
  delete_automation_rule: "Delete automation rule", create_business_role: "Create business role",
  create_form_integration: "Create form integration", update_form_integration: "Update form integration",
  delete_form_integration: "Delete form integration", set_user_access_rules: "Set user access rules",
};
function friendlyTool(tool: string): string {
  return TOOL_LABELS[tool] ?? tool.replace(/_/g, " ");
}

// Splits a step description at its first 'quoted' token: the token is the
// step's own short label (e.g. "s1"), everything after it is the "shape" —
// the part that's identical across a batch (e.g. "under parent member
// 'total'"). Steps sharing the same tool + shape collapse into one group
// in the plan below; batches with a different shape (e.g. nested under a
// different parent) stay separate, since that difference is exactly the
// kind of thing a developer reviewing the plan needs to see, not have
// hidden by over-eager collapsing.
function stepNameAndShape(description: string): { name: string; shape: string } {
  const match = description.match(/'([^']+)'/);
  if (!match || match.index === undefined) return { name: description, shape: "" };
  return { name: match[1], shape: description.slice(match.index + match[0].length).trim() };
}

type ProposalStepEntry =
  | { kind: "single"; index: number; step: AIProposalStep }
  | { kind: "group"; index: number; tool: string; shape: string; items: { index: number; name: string; step: AIProposalStep }[] };

// Groups steps for the plan preview: same tool + same shape → one row.
// Failed steps are never grouped — a failure needs full, individual detail
// to diagnose, so it's always worth its own line, mirroring the same rule
// buildExecutionSummary applies to the post-execution chat summary
// (internal/gateway/ai_handler.go). A "group" of exactly one item renders
// as a plain single step, not a "1×" batch.
function groupProposalSteps(steps: AIProposalStep[]): ProposalStepEntry[] {
  const groups = new Map<string, Extract<ProposalStepEntry, { kind: "group" }>>();
  const order: ProposalStepEntry[] = [];

  steps.forEach((step, index) => {
    if (step.status === "failed") {
      order.push({ kind: "single", index, step });
      return;
    }
    const { name, shape } = stepNameAndShape(step.description);
    const key = `${step.tool}::${shape}`;
    let g = groups.get(key);
    if (!g) {
      g = { kind: "group", index, tool: step.tool, shape, items: [] };
      groups.set(key, g);
      order.push(g);
    }
    g.items.push({ index, name, step });
  });

  return order.map(e => (e.kind === "group" && e.items.length === 1)
    ? { kind: "single" as const, index: e.items[0].index, step: e.items[0].step }
    : e);
}

function ProposalStepGroup({ tool, shape, items }: {
  tool: string;
  shape: string;
  items: { index: number; name: string; step: AIProposalStep }[];
}) {
  const [expanded, setExpanded] = useState(false);
  const allSucceeded = items.every(it => it.step.status === "success");
  const maxShown = 8;
  const shown = expanded ? items : items.slice(0, maxShown);
  const remaining = items.length - shown.length;

  return (
    <div style={{ display: "flex", alignItems: "flex-start", gap: 10 }}>
      <div style={{
        width: 22, height: 22, borderRadius: "50%",
        background: allSucceeded ? "var(--color-success-bg)" : "var(--color-brand-100)",
        display: "flex", alignItems: "center", justifyContent: "center",
        fontSize: 10, fontWeight: 700, flexShrink: 0,
        color: allSucceeded ? "var(--color-live)" : "var(--color-brand-600)",
      }}>
        {allSucceeded ? <Check size={12} /> : items.length}
      </div>
      <div style={{ flex: 1 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
          <ToolIcon tool={tool} />
          <span>{items.length}× {friendlyTool(tool)}{shape ? ` — ${shape}` : ""}</span>
        </div>
        <div style={{ marginTop: 4, display: "flex", flexWrap: "wrap", gap: 4, alignItems: "center" }}>
          {shown.map(it => (
            <code key={it.index} className="mvx-admin-mono"
              style={{ background: "var(--color-surface-subtle)", padding: "1px 6px", borderRadius: 3, fontSize: 11 }}>
              {it.name}
            </code>
          ))}
          {remaining > 0 && (
            <button onClick={() => setExpanded(true)}
              style={{ background: "none", border: "none", color: "var(--color-brand-600)", fontSize: 11, cursor: "pointer", padding: 0 }}>
              +{remaining} more
            </button>
          )}
          {expanded && items.length > maxShown && (
            <button onClick={() => setExpanded(false)}
              style={{ background: "none", border: "none", color: "var(--color-text-muted)", fontSize: 11, cursor: "pointer", padding: 0 }}>
              Show less
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

function ProposalPanel({
  proposal,
  sessionId,
  revisionId,
  onSettled,
}: {
  proposal: AIProposal;
  sessionId: string;
  revisionId?: string;
  onSettled: (messages: AIMessage[], updated: AIProposal, session?: AISession) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr]   = useState<string | null>(null);

  const isPending  = proposal.status === "pending";
  const isExecuted = proposal.status === "executed" || proposal.status === "partial";

  const act = async (action: "confirm" | "reject") => {
    setBusy(true);
    setErr(null);
    try {
      if (action === "confirm") {
        const resp = await api.aiConfirmProposal(sessionId, proposal.id, revisionId);
        onSettled(resp.messages, resp.proposal, resp.session);
      } else {
        const resp = await api.aiRejectProposal(sessionId, proposal.id);
        onSettled(resp.messages, resp.proposal);
      }
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const steps: AIProposalStep[] = proposal.steps ?? [];

  return (
    <div className="mvx-admin-object" style={{ margin: "12px 0", fontSize: 13 }}>
      {/* Header */}
      <div className="mvx-admin-object__header">
        <StatusBadge tone={isExecuted ? "success" : isPending ? "brand" : "danger"}>
          {isPending ? "Action plan" : isExecuted ? "Executed" : "Rejected"}
        </StatusBadge>
        <span className="mvx-admin-muted">
          {steps.length} step{steps.length !== 1 ? "s" : ""}
          {proposal.status !== "pending" && ` · ${proposal.status}`}
        </span>
      </div>

      {/* Steps — same-tool/same-shape runs (e.g. 18 "add member" calls)
          collapse into one group row instead of one row each; a mixed
          batch (different parents, say) still splits into separate,
          visible groups, and any failure always gets its own full row. */}
      <div className="mvx-admin-object__body" style={{ gap: 8 }}>
        {groupProposalSteps(steps).map(entry => entry.kind === "group" ? (
          <ProposalStepGroup key={entry.index} tool={entry.tool} shape={entry.shape} items={entry.items} />
        ) : (
          <div key={entry.index} style={{ display: "flex", alignItems: "flex-start", gap: 10 }}>
            <div style={{
              width: 22, height: 22, borderRadius: "50%",
              background: entry.step.status === "success" ? "var(--color-success-bg)" : entry.step.status === "failed" ? "var(--color-danger-bg)" : "var(--color-brand-100)",
              display: "flex", alignItems: "center", justifyContent: "center",
              fontSize: 11, fontWeight: 700, flexShrink: 0,
              color: entry.step.status === "success" ? "var(--color-live)" : entry.step.status === "failed" ? "var(--color-danger)" : "var(--color-brand-600)",
            }}>
              {entry.step.status === "success" ? <Check size={12} /> : entry.step.status === "failed" ? <X size={12} /> : entry.index + 1}
            </div>
            <div style={{ flex: 1 }}>
              <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
                <ToolIcon tool={entry.step.tool} />
                <span>{entry.step.description}</span>
              </div>
              <div style={{ marginTop: 2 }}>
                <code className="mvx-admin-mono" style={{ background: "var(--color-surface-subtle)", padding: "1px 5px", borderRadius: 3 }}>{entry.step.tool}</code>
              </div>
              {entry.step.result && (
                <div style={{ fontSize: 12, marginTop: 3, color: STEP_STATUS_TONE[entry.step.status ?? ""] === "danger" ? "var(--color-danger)" : "var(--color-live)" }}>
                  {entry.step.result}
                </div>
              )}
            </div>
          </div>
        ))}
      </div>

      {/* Actions */}
      {isPending && (
        <div className="mvx-admin-inline-form" style={{ padding: "10px 14px", borderTop: "1px solid var(--color-border)" }}>
          <Button variant="primary" size="sm" loading={busy} loadingLabel="Executing…" onClick={() => act("confirm")}>
            Confirm &amp; Execute
          </Button>
          <Button size="sm" disabled={busy} onClick={() => act("reject")}>
            Cancel
          </Button>
          {err && <span className="mvx-admin-error" style={{ alignSelf: "center" }}>{err}</span>}
        </div>
      )}
    </div>
  );
}

// ── Activity panel ────────────────────────────────────────────────────────────
// A same-session history of every proposal ever made (not just the pending
// one shown inline above) — the only history beyond the linear chat
// transcript. Collapsible: closed by default, fetched on demand.

const ACTIVITY_STATUS_TONE: Record<string, DesignTone> = {
  pending: "brand", confirmed: "brand", executed: "success", partial: "warning", rejected: "danger",
};

function ActivityPanel({ sessionId, onClose }: { sessionId: string; onClose: () => void }) {
  const { data: proposals = [], isLoading } = useQuery<AIProposalWithSummary[]>({
    queryKey: ["ai-proposals", sessionId],
    queryFn: () => api.aiListProposals(sessionId),
  });

  return (
    <div
      className="mvx-context-banner"
      style={{
        borderRadius: 0, borderLeft: "none", borderRight: "none", borderTop: "none",
        flexDirection: "column", alignItems: "stretch", padding: "10px 16px",
        maxHeight: 220, overflowY: "auto",
      }}
    >
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 6 }}>
        <span style={{ fontSize: 12, fontWeight: 600 }}>Activity</span>
        <IconButton aria-label="Close activity" title="Close" size={22} onClick={onClose}>
          <X size={12} />
        </IconButton>
      </div>
      {isLoading && <div className="mvx-admin-muted" style={{ fontSize: 12 }}>Loading…</div>}
      {!isLoading && proposals.length === 0 && (
        <div className="mvx-admin-muted" style={{ fontSize: 12 }}>No proposals yet in this session.</div>
      )}
      {proposals.map(p => (
        <div key={p.id} style={{ display: "flex", alignItems: "flex-start", gap: 8, padding: "6px 0", borderBottom: "1px solid var(--color-border)" }}>
          <StatusBadge tone={ACTIVITY_STATUS_TONE[p.status] ?? "brand"}>{p.status}</StatusBadge>
          <div style={{ flex: 1, fontSize: 12 }}>
            <div>{p.summary}</div>
            <div className="mvx-admin-muted" style={{ fontSize: 11, marginTop: 2 }}>
              {p.steps.length} step{p.steps.length !== 1 ? "s" : ""} · {p.created_at.slice(0, 16).replace("T", " ")}
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}

// ── Main AIAssistant component ────────────────────────────────────────────────

export function AIAssistant({ revisionId, revisionName }: { revisionId?: string; revisionName?: string } = {}) {
  const qc = useQueryClient();
  const [sessionId, setSessionId]   = useState<string | null>(null);
  const [currentSession, setCurrentSession] = useState<AISession | null>(null);
  const [messages, setMessages]     = useState<AIMessage[]>([]);
  const [pendingProposal, setPendingProposal] = useState<AIProposal | null>(null);
  const [input, setInput]           = useState("");
  const [thinking, setThinking]     = useState(false);
  const [streamingContent, setStreamingContent] = useState("");
  const [toolStatus, setToolStatus] = useState<string | null>(null);
  const [showSettings, setShowSettings] = useState(false);
  const [error, setError]           = useState<string | null>(null);
  const [hoveredSession, setHoveredSession] = useState<string | null>(null);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [documents, setDocuments]   = useState<AIDocument[]>([]);
  const [uploading, setUploading]   = useState(false);
  const [draftBusy, setDraftBusy]   = useState(false);
  const [showActivity, setShowActivity] = useState(false);
  const bottomRef = useRef<HTMLDivElement>(null);
  const inputRef  = useRef<HTMLTextAreaElement>(null);
  const fileRef   = useRef<HTMLInputElement>(null);
  const { confirm, confirmElement } = useConfirm();

  // App/model/revision the assistant is scoped to (see banner in chat area).
  const { data: demo } = useQuery({ queryKey: ["demo"], queryFn: api.getDemo });
  const { data: sessionModel } = useQuery({
    queryKey: ["dev-model", revisionId ?? ""],
    queryFn: () => api.getDevModel(revisionId || undefined),
  });

  const { data: sessions = [] } = useQuery<AISession[]>({
    queryKey: ["ai-sessions"],
    queryFn: api.aiListSessions,
  });

  const { data: settings } = useQuery<AISettings>({
    queryKey: ["ai-settings"],
    queryFn: api.aiGetSettings,
  });

  // Auto-scroll to bottom when messages change.
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  // Load session messages when switching sessions.
  const loadSession = async (id: string) => {
    setSessionId(id);
    setError(null);
    setPendingProposal(null);
    setStreamingContent("");
    setToolStatus(null);
    const data = await api.aiGetSession(id);
    setCurrentSession(data.session);
    setMessages(data.messages ?? []);
    setDocuments(data.documents ?? []);
  };

  // Create a new session.
  const newSession = useMutation({
    mutationFn: api.aiCreateSession,
    onSuccess: (sess) => {
      qc.invalidateQueries({ queryKey: ["ai-sessions"] });
      setSessionId(sess.id);
      setCurrentSession(sess);
      setMessages([]);
      setDocuments([]);
      setPendingProposal(null);
      setStreamingContent("");
      setToolStatus(null);
      setError(null);
    },
    onError: (e) => setError((e as Error).message),
  });

  // Delete a session.
  const renameSession = useMutation({
    mutationFn: ({ id, title }: { id: string; title: string }) => api.aiRenameSession(id, title),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ai-sessions"] }),
  });

  const deleteSession = useMutation({
    mutationFn: (id: string) => api.aiDeleteSession(id),
    onSuccess: (_data, id) => {
      qc.invalidateQueries({ queryKey: ["ai-sessions"] });
      if (sessionId === id) {
        setSessionId(null);
        setCurrentSession(null);
        setMessages([]);
        setDocuments([]);
        setPendingProposal(null);
        setStreamingContent("");
        setToolStatus(null);
      }
    },
  });

  // Promote the session's isolated draft revision to active. Goes through
  // the AI-specific endpoint (not the generic revision-activate one) so the
  // session's draft_revision_id is cleared server-side too — that's what
  // makes the "AI draft" banner disappear below, the only visible sign the
  // click did anything, and what makes the session's next confirmed
  // proposal start a fresh draft instead of continuing to write straight
  // into what just became the live model.
  const promoteDraft = async () => {
    if (!sessionId || !currentSession?.draft_revision_id) return;
    setDraftBusy(true);
    setError(null);
    try {
      const resp = await api.aiPromoteDraft(sessionId!);
      setCurrentSession(resp.session);
      setMessages(resp.messages ?? []);
      qc.invalidateQueries({ queryKey: ["dev-model"] });
      qc.invalidateQueries({ queryKey: ["ai-proposals", sessionId] });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setDraftBusy(false);
    }
  };

  // Goes through the AI-specific discard-draft endpoint (not the generic
  // revision-delete one) so session.draft_revision_id is cleared
  // server-side too — the previous approach only updated local React
  // state, so a page reload (or the session's next confirmed proposal)
  // still saw the dangling draft id and 500'd on an FK violation.
  const discardDraft = () => {
    if (!sessionId || !currentSession?.draft_revision_id) return;
    confirm({
      title: "Discard this draft?",
      body: "This permanently removes every change the AI made in this session. This cannot be undone.",
      confirmLabel: "Discard draft",
      onConfirm: async () => {
        setDraftBusy(true);
        setError(null);
        try {
          const resp = await api.aiDiscardDraft(sessionId);
          setCurrentSession(resp.session);
          setMessages(resp.messages ?? []);
          qc.invalidateQueries({ queryKey: ["ai-proposals", sessionId] });
        } catch (e) {
          setError((e as Error).message);
        } finally {
          setDraftBusy(false);
        }
      },
    });
  };

  // Upload a document into the current session (creates a session if needed).
  const uploadDocument = async (file: File) => {
    setUploading(true);
    setError(null);
    try {
      let sid = sessionId;
      if (!sid) {
        const sess = await api.aiCreateSession();
        qc.invalidateQueries({ queryKey: ["ai-sessions"] });
        sid = sess.id;
        setSessionId(sid);
        setMessages([]);
      }
      const doc = await api.aiUploadDocument(sid, file);
      setDocuments(prev => [...prev, doc]);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setUploading(false);
      if (fileRef.current) fileRef.current.value = "";
    }
  };

  const removeDocument = async (docId: string) => {
    if (!sessionId) return;
    try {
      await api.aiDeleteDocument(sessionId, docId);
      setDocuments(prev => prev.filter(d => d.id !== docId));
    } catch (e) {
      setError((e as Error).message);
    }
  };

  // Auto-continue: when a confirmed proposal executes ALL 50 steps (the
  // server-side batch cap), the plan almost certainly has more batches —
  // send the follow-up automatically instead of making the developer type
  // "continue" between every batch. Each batch still lands as a proposal
  // needing explicit confirmation; this only removes the typing. Capped so
  // a model that keeps emitting full batches can't loop forever; any manual
  // message resets the budget.
  const autoContinueRounds = useRef(0);
  const AUTO_CONTINUE_LIMIT = 20;
  const AUTO_CONTINUE_PROMPT =
    "Continue with the remaining steps of the task. If the task is already complete, say so and stop proposing.";

  // Send a message.
  const sendMessage = async () => {
    const content = input.trim();
    if (!content || thinking) return;
    autoContinueRounds.current = 0; // manual input resets the auto budget

    // Ensure we have a session.
    let sid = sessionId;
    if (!sid) {
      if (!settings?.has_key && !settings?.tenant_key && !import.meta.env.VITE_OPENAI_API_KEY) {
        setShowSettings(true);
        setError("Set your API key in Settings before chatting.");
        return;
      }
      try {
        const sess = await api.aiCreateSession();
        qc.invalidateQueries({ queryKey: ["ai-sessions"] });
        sid = sess.id;
        setSessionId(sid);
      } catch (e) {
        setError((e as Error).message);
        return;
      }
    }

    setInput("");
    await sendContent(sid, content);
  };

  // sendContent runs one chat turn (streaming) for an existing session —
  // shared by manual sends and the batch auto-continue.
  const sendContent = async (sid: string, content: string) => {
    // Optimistically add the user message.
    const optimistic: AIMessage = {
      id: `opt-${Date.now()}`,
      session_id: sid,
      role: "user",
      content,
      created_at: new Date().toISOString(),
    };
    setMessages(prev => [...prev, optimistic]);
    setThinking(true);
    setError(null);

    setStreamingContent("");
    setToolStatus(null);

    try {
      await api.aiSendMessage(sid, content, (event) => {
        switch (event.type) {
          case "delta":
            setStreamingContent(prev => prev + (event.content ?? ""));
            break;
          case "tool_status":
            setToolStatus(event.tool ?? null);
            setStreamingContent(""); // a fresh LLM turn is starting
            break;
          case "proposal":
            setStreamingContent("");
            setToolStatus(null);
            setMessages(event.messages ?? []);
            setPendingProposal(event.proposal ?? null);
            if (event.session) setCurrentSession(event.session);
            qc.invalidateQueries({ queryKey: ["ai-proposals", sid] });
            break;
          case "done":
            setStreamingContent("");
            setToolStatus(null);
            setMessages(event.messages ?? []);
            setPendingProposal(null);
            if (event.session) setCurrentSession(event.session);
            break;
          case "error":
            setError(event.error ?? "Something went wrong");
            setMessages(prev => prev.filter(m => m.id !== optimistic.id));
            break;
        }
      });
      // The auto-generated title lands asynchronously shortly after the
      // first send — refetch the list now and once more after the namer's
      // window so the "Session" placeholder becomes the real name.
      qc.invalidateQueries({ queryKey: ["ai-sessions"] });
      setTimeout(() => qc.invalidateQueries({ queryKey: ["ai-sessions"] }), 5000);
    } catch (e) {
      setError((e as Error).message);
      // Remove optimistic message on failure.
      setMessages(prev => prev.filter(m => m.id !== optimistic.id));
    } finally {
      setThinking(false);
      setStreamingContent("");
      setToolStatus(null);
      inputRef.current?.focus();
    }
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      sendMessage();
    }
  };

  // ── Render ──────────────────────────────────────────────────────────────────

  if (showSettings) {
    return <SettingsPanel onClose={() => setShowSettings(false)} initialSettings={settings} />;
  }

  return (
    <div style={{ display: "flex", height: "100%", gap: 0 }}>

      {/* ── Session sidebar ── */}
      <div style={{
        width: 200, borderRight: "1px solid var(--color-border)", padding: "16px 12px",
        display: "flex", flexDirection: "column", gap: 6, flexShrink: 0,
        overflowY: "auto",
      }}>
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 8 }}>
          <span style={{ fontSize: 12, fontWeight: 600 }}>Sessions</span>
          <div style={{ display: "flex", gap: 2 }}>
            <IconButton
              aria-label="Activity"
              title="Show this session's proposal history"
              size={26}
              disabled={!sessionId}
              onClick={() => setShowActivity(v => !v)}
            >
              <History size={14} />
            </IconButton>
            <IconButton aria-label="AI settings" title="AI Settings" size={26} onClick={() => setShowSettings(true)}>
              <Settings size={14} />
            </IconButton>
          </div>
        </div>

        <Button
          size="sm"
          variant="ghost"
          leadingIcon={<Plus size={13} />}
          disabled={newSession.isPending}
          onClick={() => newSession.mutate()}
          style={{ marginBottom: 4 }}
        >
          New session
        </Button>

        {sessions.map(s => (
          <div
            key={s.id}
            onMouseEnter={() => setHoveredSession(s.id)}
            onMouseLeave={() => setHoveredSession(null)}
            style={{ position: "relative" }}
          >
            {renamingId === s.id ? (
              <input
                autoFocus
                defaultValue={s.title || ""}
                placeholder="Session name"
                className="mvx-input"
                style={{ height: 30, fontSize: 12 }}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    const v = (e.target as HTMLInputElement).value.trim();
                    if (v) renameSession.mutate({ id: s.id, title: v });
                    setRenamingId(null);
                  }
                  if (e.key === "Escape") setRenamingId(null);
                }}
                onBlur={(e) => {
                  const v = e.target.value.trim();
                  if (v && v !== (s.title || "")) renameSession.mutate({ id: s.id, title: v });
                  setRenamingId(null);
                }}
              />
            ) : (
              <button
                type="button"
                onClick={() => loadSession(s.id)}
                onDoubleClick={() => setRenamingId(s.id)}
                className={["mvx-sidebar-nav__item", s.id === sessionId ? "mvx-sidebar-nav__item--active" : ""].filter(Boolean).join(" ")}
                style={{ width: "100%", paddingRight: 52, display: "block", textAlign: "left" }}
              >
                <div style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                  {s.title || "Session"}
                </div>
                <div style={{ fontSize: 10, color: "var(--color-text-subtle)", marginTop: 1 }}>
                  {s.created_at.slice(0, 10)}
                </div>
              </button>
            )}
            {hoveredSession === s.id && renamingId !== s.id && (
              <>
                <IconButton
                  aria-label="Rename session"
                  title="Rename session"
                  size={24}
                  style={{ position: "absolute", right: 30, top: "50%", transform: "translateY(-50%)" }}
                  onClick={(e) => { e.stopPropagation(); setRenamingId(s.id); }}
                >
                  <Pencil size={12} />
                </IconButton>
                <IconButton
                  aria-label="Delete session"
                  title="Delete session"
                  danger
                  size={24}
                  style={{ position: "absolute", right: 4, top: "50%", transform: "translateY(-50%)" }}
                  onClick={(e) => { e.stopPropagation(); deleteSession.mutate(s.id); }}
                >
                  <Trash2 size={12} />
                </IconButton>
              </>
            )}
          </div>
        ))}

        {!settings?.has_key && !settings?.tenant_key && (
          <div className="mvx-context-banner mvx-context-banner--warning" style={{ marginTop: "auto", padding: "8px 10px", fontSize: 11 }}>
            <div style={{ fontWeight: 500 }}>No API key set</div>
            <Button size="sm" variant="ghost" onClick={() => setShowSettings(true)} style={{ marginTop: 2, padding: 0 }}>
              Open Settings →
            </Button>
          </div>
        )}
      </div>

      {/* ── Chat area ── */}
      <div style={{ flex: 1, display: "flex", flexDirection: "column", minWidth: 0 }}>

        {/* What model this session is about — a session is app+revision
            scoped, and with several models per app the developer had no way
            to tell which one the assistant would touch (reported live). */}
        {sessionId && (
          <div className="mvx-admin-muted" style={{ fontSize: 11, padding: "6px 24px", borderBottom: "1px solid var(--color-border)", display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
            <span>Working on model:</span>
            <strong style={{ color: "var(--color-text)" }}>{sessionModel?.model_name ?? "…"}</strong>
            <span>· revision:</span>
            <strong style={{ color: "var(--color-text)" }}>{revisionName || demo?.revision || "active"}</strong>
            {currentSession?.draft_revision_id && <span>· writes go to this session's isolated draft revision</span>}
          </div>
        )}

        {showActivity && sessionId && (
          <ActivityPanel sessionId={sessionId} onClose={() => setShowActivity(false)} />
        )}

        {/* Draft revision banner — shown once the session's first confirmed
            proposal has created an isolated draft. AI writes always land here,
            never in the live model, until explicitly promoted. */}
        {currentSession?.draft_revision_id && (
          <div className="mvx-context-banner" style={{ display: "flex", alignItems: "center", gap: 10, borderRadius: 0, borderLeft: "none", borderRight: "none", borderTop: "none" }}>
            <RevisionBadge status="draft" label="AI draft" />
            <span style={{ flex: 1, fontSize: 12 }}>
              Working in an isolated draft revision — changes here won't affect the live model until promoted.
            </span>
            <Button size="sm" variant="primary" loading={draftBusy} loadingLabel="Working…" onClick={promoteDraft}>
              Promote to Active
            </Button>
            <Button size="sm" disabled={draftBusy} onClick={discardDraft}>
              Discard
            </Button>
          </div>
        )}

        {/* Messages */}
        <div style={{ flex: 1, overflowY: "auto", padding: "20px 24px" }}>
          {messages.length === 0 && !sessionId && (
            <div style={{ textAlign: "center", color: "var(--color-text-muted)", marginTop: 60 }}>
              <Bot size={32} aria-hidden="true" style={{ marginBottom: 12, color: "var(--color-text-subtle)" }} />
              <div style={{ fontSize: 15, fontWeight: 500, color: "var(--color-text)", marginBottom: 6 }}>AI Developer</div>
              <div style={{ fontSize: 13, maxWidth: 340, margin: "0 auto", lineHeight: 1.6 }}>
                I build your model with you — creating and changing metrics, dimensions,
                grids, dashboards, workflows, and forms — and answer anything about it.
                Every change lands as a plan you confirm first. Start a new session to begin.
              </div>
            </div>
          )}

          {messages.filter(m => m.role !== "tool").map(msg => (
            <MessageBubble key={msg.id} msg={msg} />
          ))}

          {/* Pending proposal — show after the last assistant message */}
          {pendingProposal && sessionId && (
            <ProposalPanel
              proposal={pendingProposal}
              sessionId={sessionId}
              revisionId={revisionId}
              onSettled={(msgs, updated, session) => {
                setMessages(msgs);
                setPendingProposal(updated.status === "pending" ? updated : null);
                if (session) setCurrentSession(session);
                qc.invalidateQueries({ queryKey: ["ai-proposals", sessionId] });
                // A fully-executed maximum-size batch means the plan very
                // likely continues — ask for the next batch automatically.
                if (
                  updated.status === "executed" &&
                  (updated.steps?.length ?? 0) >= 50 &&
                  autoContinueRounds.current < AUTO_CONTINUE_LIMIT
                ) {
                  autoContinueRounds.current += 1;
                  void sendContent(sessionId, AUTO_CONTINUE_PROMPT);
                }
              }}
            />
          )}

          {thinking && streamingContent && (
            <div className="mvx-chat-row">
              <div className="mvx-chat-bubble">{streamingContent}</div>
            </div>
          )}

          {thinking && !streamingContent && (
            <div className="mvx-chat-row">
              <div className="mvx-chat-bubble mvx-chat-bubble--pending">
                <span style={{ animation: "pulse 1s infinite" }}>
                  {toolStatus ? `Calling ${toolStatus}…` : "Thinking…"}
                </span>
              </div>
            </div>
          )}

          {error && <p className="mvx-admin-error" style={{ marginBottom: 8 }}>{error}</p>}

          <div ref={bottomRef} />
        </div>

        {/* Input bar */}
        <div style={{ padding: "12px 24px 20px", borderTop: "1px solid var(--color-border)" }}>
          {/* Attached document chips */}
          {(documents.length > 0 || uploading) && (
            <div style={{ display: "flex", flexWrap: "wrap", gap: 6, marginBottom: 8 }}>
              {documents.map(d => (
                <FilterChip
                  key={d.id}
                  title={`${d.char_count.toLocaleString()} chars extracted${d.truncated ? " (truncated)" : ""}`}
                  onClear={() => removeDocument(d.id)}
                >
                  <FileText size={11} aria-hidden="true" /> {d.filename}{d.truncated ? " ⚠" : ""}
                </FilterChip>
              ))}
              {uploading && (
                <span className="mvx-admin-muted" style={{ padding: "3px 4px" }}>Uploading…</span>
              )}
            </div>
          )}
          <div style={{ display: "flex", gap: 10, alignItems: "flex-end" }}>
            <input
              ref={fileRef}
              type="file"
              accept=".pdf,.xlsx,.xlsm,.docx,.csv,.txt,.md,.json,.log"
              style={{ display: "none" }}
              onChange={e => { const f = e.target.files?.[0]; if (f) uploadDocument(f); }}
            />
            <IconButton
              aria-label="Attach a document"
              title="Attach a document (pdf, xlsx, docx, csv, txt, md)"
              size={42}
              disabled={uploading || thinking}
              onClick={() => fileRef.current?.click()}
            >
              <Paperclip size={16} />
            </IconButton>
            <Textarea
              ref={inputRef}
              value={input}
              onChange={e => setInput(e.target.value)}
              onKeyDown={onKeyDown}
              placeholder={sessionId ? "Ask about your model… (Enter to send, Shift+Enter for newline)" : "Start a new session to begin chatting"}
              disabled={thinking}
              rows={2}
              style={{ flex: 1, resize: "none" }}
              aria-label="Message"
            />
            <Button
              variant="primary"
              disabled={!input.trim() || thinking}
              onClick={sendMessage}
              style={{ flexShrink: 0, height: 42 }}
            >
              Send
            </Button>
          </div>
          <div className="mvx-admin-muted" style={{ marginTop: 6, fontSize: 11 }}>
            Provider: {settings?.provider ?? "openai"} · Model: {settings?.model ?? "gpt-4o-mini"}
          </div>
        </div>
      </div>
      {confirmElement}
    </div>
  );
}
