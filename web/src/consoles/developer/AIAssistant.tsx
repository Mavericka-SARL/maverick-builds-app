import React, { useState, useRef, useEffect, type ClipboardEvent } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Archive, ArrowUpDown, Bot, Bookmark, Check, ClipboardList, Download, FileSpreadsheet, FileText, History, LayoutDashboard, LayoutGrid, Link2, ListTree, Paperclip,
  Pencil, Plus, Puzzle, Rocket, Ruler, Search, Settings, Table2, Trash2, Users, Workflow, Wrench, X, Zap,
} from "lucide-react";
import { ExportDownloadButton } from "../ExportDownloadButton";
import { downloadBlob } from "../business/blobUtils";
import { credentialSteps, credentialsComplete } from "./proposalCredentials";
import { api, type AIConversion, type AISession, type AIMessage, type AISettings, type AIProposal, type AIProposalStep, type AIProposalWithSummary, type AIDocument, type AISessionUsage, type AITaskPlan, type AITaskStage, type AIEarlierPlan, type AIMemoryStatus, type AITurnStatus } from "../../api/client";
import { AttachPlanPanel, EarlierPlansCard, MemoryGauge, MemoryPanel } from "./AIMemoryPlan";
import { ArchitectureScheme, PlanReviewCard } from "./AIArchitecture";
import { useDemo } from "../../api/useDemo";
import {
  Button, IconButton, TextInput, Select, Textarea, Field, StatusBadge,
  FilterChip, InlineAlert, SectionHeader, useConfirm, type DesignTone,
  Txt,
} from "../../ui";
import { parsePastedTable } from "./pastedTable";
import { buildAdvice } from "./aiBuildAdvice";

// ── Settings panel ────────────────────────────────────────────────────────────
// The model is free text, offered the models the key reaches (asked from the
// provider, so a new model appears the day it ships): a name the key cannot
// use otherwise failed only on the first call. Blank means the gateway's
// default for the provider (providerDefaultModels in
// internal/gateway/ai_handler.go).

function SettingsPanel({ onClose, initialSettings }: { onClose: () => void; initialSettings?: AISettings }) {
  const qc = useQueryClient();

  // Initialize from already-loaded settings passed from parent — no sync effect needed.
  const [provider, setProvider] = useState(initialSettings?.provider ?? "openai");
  const [model, setModel]       = useState(initialSettings?.model ?? "");
  const [apiKey, setApiKey]     = useState("");
  const [saved, setSaved]       = useState(false);
  const [effort, setEffort]     = useState(initialSettings?.reasoning_effort ?? "");
  // The developer's own daily call limit (owner, 2026-11-16): blank is
  // none of their own, so the server's default applies.
  const [limit, setLimit]       = useState(initialSettings?.daily_call_limit ? String(initialSettings.daily_call_limit) : "");
  const limitN = limit.trim() === "" ? null : Number(limit.trim());
  const limitBad = limitN !== null && (!Number.isInteger(limitN) || limitN < 1 || limitN > 1_000_000);
  // The models the key reaches: asked for the saved key when the panel opens
  // or the provider changes, and for a typed key on "List models".
  const [typedKeyAsked, setTypedKeyAsked] = useState("");
  const models = useQuery({
    queryKey: ["ai-models", provider, typedKeyAsked],
    queryFn: () => api.aiListModels({ provider, api_key: typedKeyAsked || undefined }),
    staleTime: 5 * 60_000,
  });
  const offered = Array.isArray(models.data?.models) ? models.data.models : [];

  // A model name belongs to one provider, so switching provider clears it
  // (blank = that provider's default) — inline in the handler, not an effect.
  const handleProviderChange = (newProvider: string) => {
    setProvider(newProvider);
    setModel("");
  };

  const save = useMutation({
    mutationFn: () => api.aiSaveSettings({ provider, model, api_key: apiKey || undefined, reasoning_effort: effort, daily_call_limit: limitN }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["ai-settings"] });
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    },
  });

  const test = useMutation({
    mutationFn: () => api.aiTestSettings({ provider, model, api_key: apiKey || undefined, reasoning_effort: effort }),
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
            Your administrator supplies the AI key for this tenant (<Txt>{initialSettings.tenant_provider}</Txt><Txt>), and it is used for
            every call. Anything you set here is ignored.</Txt>
          </InlineAlert>
        ) : initialSettings?.tenant_key ? (
          <InlineAlert tone="info">
            Your administrator supplies an AI key for this tenant (<Txt>{initialSettings.tenant_provider}</Txt><Txt>). You only need a key
            here if you want your calls billed to your own account instead.</Txt>
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

        <Field
          label="Model"
          description={offered.length
            ? `Any model ID your provider offers; the list holds the ${offered.length} your key reaches.`
            : models.data?.error
              ? `Any model ID your provider offers (the key's models could not be listed: ${models.data.error}).`
              : "Any model ID your provider offers, including ones released after this screen was built."}
        >
          <TextInput
            value={model}
            onChange={e => setModel(e.target.value)}
            placeholder={initialSettings?.default_models?.[provider] ? `Blank runs ${initialSettings.default_models[provider]}` : "Leave blank for the provider's default"}
            aria-label="Model"
            autoComplete="off"
            list="ai-model-options"
          />
          <datalist id="ai-model-options">
            {offered.map(m => <option key={m} value={m} />)}
          </datalist>
        </Field>

        {provider === "openai" && (
          <Field
            label="Reasoning effort"
            description="Sent with every call to a reasoning model. Models that take tools with reasoning only through OpenAI's Responses API are sent there, so every effort works; a lower effort makes turns shorter, a higher one plans builds better."
          >
            <Select value={effort} onChange={e => setEffort(e.target.value)} aria-label="Reasoning effort">
              <option value="">Model default</option>
              <option value="none">None</option>
              <option value="minimal">Minimal</option>
              <option value="low">Low</option>
              <option value="medium">Medium</option>
              <option value="high">High</option>
              <option value="xhigh">Extra high</option>
            </Select>
          </Field>
        )}

        {buildAdvice(provider, model || initialSettings?.default_models?.[provider] || "", effort) && (
          <p role="note" style={{ fontSize: 12, margin: 0, color: "var(--color-warning, var(--text-secondary))" }}>
            {buildAdvice(provider, model || initialSettings?.default_models?.[provider] || "", effort)}
          </p>
        )}

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

        <Field
          label="Daily call limit"
          description={dailyLimitNote(initialSettings)}
          error={limitBad ? "A whole number from 1 to 1,000,000, or blank." : undefined}
        >
          <TextInput
            value={limit}
            onChange={e => setLimit(e.target.value)}
            inputMode="numeric"
            aria-label="Daily call limit"
            placeholder={initialSettings?.server_daily_call_limit ? `${initialSettings.server_daily_call_limit} (this server's default)` : "calls a day"}
            style={{ width: 220 }}
          />
        </Field>

        <div className="mvx-admin-inline-form">
          <Button
            variant="primary"
            leadingIcon={saved ? <Check size={14} /> : undefined}
            disabled={limitBad}
            loading={save.isPending}
            loadingLabel="Saving…"
            onClick={() => save.mutate()}
          >
            {saved ? "Saved" : "Save settings"}
          </Button>
          <Button loading={test.isPending} loadingLabel="Testing…" onClick={() => test.mutate()}>
            Test connection
          </Button>
          {apiKey && apiKey !== typedKeyAsked && (
            <Button loading={models.isFetching} loadingLabel="Listing…" onClick={() => setTypedKeyAsked(apiKey)}>
              List models
            </Button>
          )}
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

// dailyLimitNote says what the developer's daily call limit does next to the
// others: the server's default, the tenant admin's, the server's own key.
function dailyLimitNote(s?: AISettings): string {
  const server = s?.server_daily_call_limit;
  const parts = [`Your model calls per day (UTC). Blank uses this server's default${server ? ` of ${server.toLocaleString()}` : ""}; set more for a long build, or less to cap your spend.`];
  if (s?.tenant_daily_call_limit) {
    parts.push(`Your tenant admin's limit is ${s.tenant_daily_call_limit.toLocaleString()} a day for each developer; the lower one counts.`);
  }
  if (s?.daily_limit_by === "server_key") {
    parts.push(`Calls on this server's own AI key stop at ${server?.toLocaleString() ?? "its limit"} a day, whatever you set.`);
  }
  return parts.join(" ");
}

// The files the AI Developer takes as attachments.
const ATTACHABLE = ".pdf,.xlsx,.xlsm,.docx,.csv,.txt,.md,.json,.log";

// The AI Developer's open session, remembered for this browser tab so a
// reload opens it again (owner, 2026-11-13).
const OPEN_SESSION_KEY = "ai_open_session";
function rememberOpenSession(id: string) {
  try { sessionStorage.setItem(OPEN_SESSION_KEY, id); } catch { /* storage unavailable */ }
}

// ── Message list ─────────────────────────────────────────────────────────────

// What the AI read between two messages goes on one compact line (owner,
// 2026-11-13: a bubble per "Fetching: list_metrics" took a screen): the
// tools in order, a repeat counted ("read attached sheet ×2").
function ToolsLine({ names }: { names: string[] }) {
  const counted: { name: string; n: number }[] = [];
  for (const n of names) {
    const last = counted.at(-1);
    if (last && last.name === n) last.n += 1;
    else counted.push({ name: n, n: 1 });
  }
  const label = (n: string) => (n === "propose_actions" ? "wrote a plan" : n.replace(/_/g, " "));
  return (
    <div className="mvx-admin-muted" style={{ fontSize: 11, margin: "2px 0 8px", display: "flex", alignItems: "baseline", gap: 6 }}>
      <Search size={11} aria-hidden="true" style={{ flexShrink: 0, position: "relative", top: 1 }} />
      <Txt>{counted.map(c => (c.n > 1 ? `${label(c.name)} ×${c.n}` : label(c.name))).join(" · ")}</Txt>
    </div>
  );
}

// MessageList renders the session's messages: what each side wrote as
// bubbles, the AI's reads between them as one ToolsLine. The first item of
// the AI's latest answer carries data-turn-start, for scrolling to it.
function MessageList({ messages }: { messages: AIMessage[] }) {
  const lastUser = messages.reduce((at, m, i) => (m.role === "user" ? i : at), -1);
  const items: React.ReactNode[] = [];
  let tools: string[] = [];
  let toolsKey = "";
  let toolsAt = -1;
  let marked = false;
  const mark = (i: number) => {
    if (marked || i <= lastUser) return {};
    marked = true;
    return { "data-turn-start": true };
  };
  const flush = () => {
    if (tools.length > 0) {
      items.push(<div key={`tools-${toolsKey}`} {...mark(toolsAt)}><ToolsLine names={tools} /></div>);
      tools = [];
    }
  };
  messages.forEach((m, i) => {
    if (m.role === "tool") return;
    if (m.content.trim() !== "") {
      flush();
      items.push(<div key={m.id} {...mark(i)}><MessageBubble msg={{ ...m, tool_calls: undefined }} /></div>);
    }
    if (m.tool_calls && m.tool_calls.length > 0) {
      if (tools.length === 0) {
        toolsKey = m.id;
        toolsAt = i;
      }
      tools.push(...m.tool_calls.map(tc => tc.name));
    }
  });
  flush();
  return <>{items}</>;
}

// ── Message bubble ────────────────────────────────────────────────────────────

// A turn that went on by itself after a confirmed plan starts with this
// (aiassistant.ContinuePrefix): shown as an automatic line, not a message
// the developer typed.
const CONTINUE_PREFIX = "▶ ";

function MessageBubble({ msg }: { msg: AIMessage }) {
  if (msg.role === "tool") return null; // tool results are internal

  const isUser = msg.role === "user";
  if (isUser && msg.content.startsWith(CONTINUE_PREFIX)) {
    const approved = msg.content.startsWith(CONTINUE_PREFIX + "Plan approved");
    return (
      <div className="mvx-admin-muted" role="note" title={msg.content.slice(CONTINUE_PREFIX.length)}
        style={{ fontSize: 12, textAlign: "center", margin: "6px 0" }}>
        <Txt>{approved ? "▶ Plan approved — the AI starts building" : "▶ Plan confirmed — the AI goes on with the next step"}</Txt>
      </div>
    );
  }
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
    case "create_dimension":     return <ListTree {...props} />;
    case "add_dimension_member":
    case "generate_time_members":
    case "add_dimension_property": return <Plus {...props} />;
    case "update_dimension":
    case "update_dimension_member":
    case "update_dimension_property":
    case "update_grid":
    case "update_grid_dimension":
    case "update_dashboard":
    case "update_dashboard_widget":
    case "update_dashboard_folder":
    case "update_business_role": return <Pencil {...props} />;
    case "reorder_dimension_members": return <ArrowUpDown {...props} />;
    case "reorder_grid_metrics": return <ArrowUpDown {...props} />;
    case "reorder_dashboards": return <ArrowUpDown {...props} />;
    case "arrange_dashboard": return <LayoutGrid {...props} />;
    case "set_date_format": return <Settings {...props} />;
    case "create_grid":
    case "add_grid_metric":
    case "add_grid_dimension":   return <Table2 {...props} />;
    case "create_dashboard":
    case "create_dashboard_folder": return <LayoutDashboard {...props} />;
    case "add_dashboard_widget": return <Puzzle {...props} />;
    case "create_revision":      return <Bookmark {...props} />;
    case "create_workflow_def":
    case "update_workflow_def":
    case "restore_workflow_def":
    case "publish_workflow_def":
    case "duplicate_workflow_def": return <Workflow {...props} />;
    case "archive_workflow_def": return <Archive {...props} />;
    case "delete_metric":
    case "delete_dimension":
    case "delete_dimension_member":
    case "delete_dimension_property":
    case "delete_grid":
    case "remove_grid_metric":
    case "remove_grid_dimension":
    case "delete_dashboard":
    case "delete_dashboard_widget":
    case "delete_dashboard_folder":
    case "delete_business_role":
    case "delete_workflow_def":
    case "delete_form_def":
    case "delete_automation_rule":
    case "delete_form_integration": return <Trash2 {...props} />;
    case "create_form_def":
    case "update_form_def":      return <ClipboardList {...props} />;
    case "create_automation_rule":
    case "update_automation_rule": return <Zap {...props} />;
    case "create_business_role":
    case "set_role_dashboards":  return <Users {...props} />;
    case "backfill_form_integration": return <Rocket {...props} />;
    case "create_form_integration":
    case "update_form_integration": return <Link2 {...props} />;
    default:                     return <Wrench {...props} />;
  }
}

const STEP_STATUS_TONE: Record<string, DesignTone> = { success: "success", failed: "danger" };

const TOOL_LABELS: Record<string, string> = {
  create_metric: "Create metric", update_metric: "Update metric", delete_metric: "Delete metric",
  create_dimension: "Create dimension", update_dimension: "Update dimension", add_dimension_member: "Add dimension member",
  update_dimension_member: "Update dimension member", reorder_dimension_members: "Reorder dimension members",
  add_dimension_property: "Add dimension property",
  update_dimension_property: "Update dimension property", delete_dimension_property: "Delete dimension property",
  create_grid: "Create grid", add_grid_metric: "Add grid metric", add_grid_dimension: "Add grid dimension",
  create_dashboard: "Create dashboard", add_dashboard_widget: "Add dashboard widget", set_tags: "Set tags",
  create_revision: "Create revision",
  delete_dimension: "Delete dimension", delete_dimension_member: "Delete dimension member",
  generate_time_members: "Generate time periods", update_grid: "Rename grid", delete_grid: "Delete grid",
  remove_grid_metric: "Remove grid metric", reorder_grid_metrics: "Reorder grid metrics", reorder_dashboards: "Reorder dashboards", arrange_dashboard: "Arrange dashboard", set_date_format: "Set date format", remove_grid_dimension: "Remove grid dimension",
  update_grid_dimension: "Set grid display level", create_dashboard_folder: "Create dashboard folder",
  update_dashboard_folder: "Update dashboard folder", delete_dashboard_folder: "Delete dashboard folder",
  update_dashboard: "Update dashboard", delete_dashboard: "Delete dashboard",
  update_dashboard_widget: "Update dashboard widget", delete_dashboard_widget: "Delete dashboard widget",
  publish_workflow_def: "Publish workflow",
  archive_workflow_def: "Archive workflow", restore_workflow_def: "Restore workflow",
  duplicate_workflow_def: "Duplicate workflow", update_business_role: "Rename business role",
  delete_business_role: "Delete business role", set_role_dashboards: "Set role dashboards",
  backfill_form_integration: "Post records",
  create_workflow_def: "Create workflow", update_workflow_def: "Update workflow", delete_workflow_def: "Delete workflow",
  create_form_def: "Create table", update_form_def: "Update table", delete_form_def: "Delete table",
  create_automation_rule: "Create automation rule", update_automation_rule: "Update automation rule",
  delete_automation_rule: "Delete automation rule", create_business_role: "Create business role",
  create_form_integration: "Create table integration", update_form_integration: "Update table integration",
  delete_form_integration: "Delete table integration", set_user_access_rules: "Set user access rules",
  create_file_integration: "Create Excel/CSV integration", import_file_data: "Import attached file", write_input_values: "Write input values",
  create_export_integration: "Create data export", update_integration: "Update integration",
  delete_integration: "Delete integration",
  create_api_integration: "Create REST API connector", update_api_integration: "Update REST API connector",
  create_connection: "Create sign-in connection", update_connection: "Update sign-in connection",
  run_integration: "Run integration", trigger_automation_rule: "Trigger automation rule",
  rename_draft: "Rename the AI draft",
  recalculate_revision: "Recalculate revision", clear_input_values: "Clear values", fill_sample_values: "Fill with sample values", save_records: "Save records", delete_records: "Delete records",
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
    if (step.status === "failed" || step.status === "skipped") {
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
          <span>{items.length}× {friendlyTool(tool)}<Txt>{shape ? ` — ${shape}` : ""}</Txt></span>
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
  // Sign-in details typed here go with the confirmation and nowhere else.
  const credSteps = credentialSteps(proposal.steps ?? []);
  const [secrets, setSecrets] = useState<Record<number, Record<string, string>>>(() =>
    Object.fromEntries(credSteps.map(c => [c.number, { ...c.prefill }])));
  const credsReady = credentialsComplete(credSteps, secrets);

  const isPending  = proposal.status === "pending";
  const isExecuted = proposal.status === "executed" || proposal.status === "partial";

  const act = async (action: "confirm" | "reject") => {
    setBusy(true);
    setErr(null);
    try {
      if (action === "confirm") {
        const resp = await api.aiConfirmProposal(sessionId, proposal.id, revisionId, credSteps.length > 0 ? secrets : undefined);
        setSecrets({});
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
          {steps.length} step<Txt>{steps.length !== 1 ? "s" : ""}</Txt>
          <Txt>{proposal.status !== "pending" && ` · ${proposal.status}`}</Txt>
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

      {/* Sign-in details: the assistant never sees these */}
      {isPending && credSteps.length > 0 && (
        <div style={{ padding: "10px 14px", borderTop: "1px solid var(--color-border)", display: "grid", gap: 10 }}>
          <div className="mvx-admin-muted" style={{ fontSize: 12 }}>
            Sign-in details go to the server with your confirmation. The assistant does not see them and they are not
            kept in this chat.
          </div>
          {credSteps.map(c => (
            <fieldset key={c.number} style={{ border: "1px solid var(--color-border)", borderRadius: 6, padding: "8px 10px", margin: 0, display: "grid", gap: 6 }}>
              <legend style={{ fontSize: 12, padding: "0 4px" }}>Step {c.number}: {c.name} ({c.authType})</legend>
              {c.fields.map(f => {
                const value = secrets[c.number]?.[f.key] ?? "";
                const set = (v: string) => setSecrets(prev => ({ ...prev, [c.number]: { ...(prev[c.number] ?? {}), [f.key]: v } }));
                const label = `${f.label}${f.optional ? " (optional)" : ""}`;
                return (
                  <Field key={f.key} label={label}>
                    {f.multiline
                      ? <Textarea rows={4} value={value} onChange={e => set(e.target.value)} aria-label={`${label} for step ${c.number}`} spellCheck={false} autoComplete="off" />
                      : <TextInput type={f.secret ? "password" : "text"} value={value} onChange={e => set(e.target.value)} aria-label={`${label} for step ${c.number}`} autoComplete="off" />}
                  </Field>
                );
              })}
            </fieldset>
          ))}
        </div>
      )}

      {/* Actions */}
      {isPending && (
        <div className="mvx-admin-inline-form" style={{ padding: "10px 14px", borderTop: "1px solid var(--color-border)" }}>
          <Button variant="primary" size="sm" loading={busy} loadingLabel="Executing…" onClick={() => act("confirm")}
            disabled={!credsReady} title={credsReady ? undefined : "Fill in the sign-in details above first"}>
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

// ── Exports from this session ─────────────────────────────────────────────────
// Every data export this session's confirmed proposals created, each with a
// Download button. A confirmed proposal leaves the chat at once (only its
// summary message stays), so the downloads live here, fed by the session's
// proposal history: they survive a reload and refresh on every confirm.
// Collapsed under "Show exports (n)" beside the session's files.

type SessionExport = { id: string; name: string };

function useSessionExports(sessionId: string | null): SessionExport[] {
  const { data: proposals = [] } = useQuery<AIProposalWithSummary[]>({
    queryKey: ["ai-proposals", sessionId],
    queryFn: () => api.aiListProposals(sessionId as string),
    enabled: !!sessionId,
  });
  return proposals.flatMap(p => (p.steps ?? [])
    .filter(s => s.tool === "create_export_integration" && s.status === "success" && s.created_id)
    .map(s => ({ id: s.created_id as string, name: String(s.params?.name ?? s.description) })));
}

function SessionExports({ exports }: { exports: SessionExport[] }) {
  return (
    <div data-testid="session-exports" style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 8, marginBottom: 8 }}>
      {exports.map(e => (
        <span key={e.id} style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
          <span style={{ fontSize: 13 }}>{e.name}</span>
          <ExportDownloadButton integrationId={e.id} />
        </span>
      ))}
    </div>
  );
}

// ── Converted files ───────────────────────────────────────────────────────────
// Attachments the assistant converted to the layout an import reads
// (prepare_converted_file), each downloadable as CSV or Excel. The server
// rebuilds the file from the attachment on every download.

function SessionConversions({ sessionId, conversions }: { sessionId: string; conversions: AIConversion[] }) {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const download = async (c: AIConversion, format: "csv" | "xlsx") => {
    setBusy(c.id + format);
    setError(null);
    try {
      const { blob, filename } = await api.aiDownloadConversion(sessionId, c.id, format);
      downloadBlob(blob, filename);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Download failed");
    } finally {
      setBusy(null);
    }
  };
  return (
    <div data-testid="session-conversions" style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 8, marginBottom: 8 }}>
      {conversions.map(c => (
        <span key={c.id} style={{ display: "inline-flex", alignItems: "center", gap: 6 }} title={c.columns.join(", ")}>
          <span style={{ fontSize: 13 }}>{c.filename}</span>
          <span className="mvx-admin-muted" style={{ fontSize: 12 }}>{c.row_count} row<Txt>{c.row_count !== 1 ? "s" : ""}</Txt></span>
          <Button size="sm" leadingIcon={<Download size={13} />} loading={busy === c.id + "csv"} loadingLabel="Preparing…"
            onClick={() => download(c, "csv")} aria-label={`Download ${c.filename} as CSV`}>CSV</Button>
          <Button size="sm" leadingIcon={<Download size={13} />} loading={busy === c.id + "xlsx"} loadingLabel="Preparing…"
            onClick={() => download(c, "xlsx")} aria-label={`Download ${c.filename} as Excel`}>Excel</Button>
        </span>
      ))}
      {error && <span className="mvx-admin-error" style={{ fontSize: 12 }}>{error}</span>}
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
              {p.steps.length} step<Txt>{p.steps.length !== 1 ? "s" : ""}</Txt> <Txt>· </Txt>{p.created_at.slice(0, 16).replace("T", " ")}
            </div>
            {/* Kept with the proposal, so a reopened session still shows them. */}
            {(p.warnings?.length ?? 0) > 0 && (
              <div role="note" style={{ fontSize: 11, marginTop: 4, color: "var(--color-warning-text)" }}>
                <strong>The plan check warned:</strong>
                <ul style={{ margin: "2px 0 0", paddingLeft: 16 }}>
                  {p.warnings!.map((w, i) => <li key={i}>{w}</li>)}
                </ul>
              </div>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}


// formatElapsed is a running turn's time, m:ss.
function formatElapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

// formatStep is a finished step's time: tenths of a second under ten
// seconds, m:ss above.
function formatStep(ms: number): string {
  return ms < 10_000 ? `${(ms / 1000).toFixed(1)}s` : formatElapsed(ms);
}

// statusQuietWarnS: no word from the server for this long, while the server
// sends its status every 5 s, means the stream is gone.
const statusQuietWarnS = 20;

// TurnStatusBubble is what a running turn shows: the step it is on and how
// long that step and the turn have run; for a call to the AI, how far the
// provider has got, how long it has said nothing and when it is asked again;
// the steps before; and when the server last said anything. A server too old
// to send a status gets the one-line label it always had.
function TurnStatusBubble({ progress, toolStatus, status, heardAt, now, elapsed }: {
  progress: { tool?: string; chars?: number; steps?: number } | null;
  toolStatus: string | null;
  status: { s: AITurnStatus; at: number } | null;
  heardAt: number;
  now: number;
  elapsed: string;
}) {
  const kb = (chars?: number) => Math.max(1, Math.round((chars ?? 0) / 1024));
  const s = status?.s;
  const since = status ? Math.max(0, (now - status.at) / 1000) : 0;
  const stepS = s ? s.step_s + since : 0;
  let head: string;
  if (progress?.tool === "propose_actions") {
    head = `Writing the proposal — ${progress.steps ?? 0} step${progress.steps === 1 ? "" : "s"} so far (${kb(progress.chars)} KB)`;
  } else if (progress?.tool) {
    head = `Writing ${progress.tool} (${kb(progress.chars)} KB)`;
  } else if (s?.phase === "ai") {
    head = `Waiting for the AI — call ${s.call ?? "?"}${(s.attempt ?? 1) > 1 ? ", asked again" : ""}`;
  } else if (s?.phase === "server" && s.label) {
    head = /^[a-z_]+$/.test(s.label) ? `Running ${s.label}` : s.label.charAt(0).toUpperCase() + s.label.slice(1);
  } else {
    head = toolStatus ? `Calling ${toolStatus}` : "Thinking";
  }

  const details: string[] = [];
  if (s?.phase === "ai") {
    const sent = s.context_tokens ? ` (about ${Math.round(s.context_tokens / 1000)}k tokens)` : "";
    switch (s.stage) {
      case "sending":
        details.push(`Request sent${sent}; the AI provider has not answered yet.`);
        break;
      case "connected":
        details.push(`The AI provider accepted the request${sent}; nothing written yet.`);
        break;
      case "reasoning":
        details.push(`The AI is reasoning${sent}. The provider shows nothing of it until the AI starts writing.`);
        break;
      case "writing":
        details.push("The AI is writing.");
        break;
    }
    const quiet = (s.quiet_s ?? 0) + since;
    if (s.idle_limit_s && quiet >= 10) {
      details.push(`Nothing from the AI for ${formatElapsed(quiet * 1000)}; at ${formatElapsed(s.idle_limit_s * 1000)} it is stopped and asked again${(s.attempt ?? 1) > 1 ? " — this is the second time, the last" : ""}.`);
    }
    if (s.note) details.push(`Before: ${s.note}.`);
  }
  const heardS = heardAt ? Math.max(0, (now - heardAt) / 1000) : 0;
  const lost = status != null && heardS >= statusQuietWarnS;

  return (
    <div className="mvx-chat-bubble mvx-chat-bubble--pending" style={{ maxWidth: 680 }}>
      <div>
        <span style={lost ? undefined : { animation: "pulse 1s infinite" }}>{head}…</span>
        {s && <span className="mvx-admin-muted" style={{ marginLeft: 8 }}>{formatElapsed(stepS * 1000)} this step</span>}
        {elapsed && <span className="mvx-admin-muted" style={{ marginLeft: 8 }}>{s ? `· ${elapsed} turn` : elapsed}</span>}
      </div>
      {details.map((d) => <div key={d} className="mvx-admin-muted" style={{ fontSize: 12, marginTop: 4 }}>{d}</div>)}
      {s && s.recent.length > 0 && (
        <div className="mvx-admin-muted" style={{ fontSize: 12, marginTop: 4 }}>
          {`Done before: ${s.recent.map((r) => `${r.label} ${formatStep(r.ms)}`).join(" · ")}`}
        </div>
      )}
      {status && (
        lost ? (
          <div style={{ fontSize: 12, marginTop: 6, color: "var(--color-warning)" }}>
            {`No word from the server for ${formatElapsed(heardS * 1000)}, though it reports every 5 seconds: the connection is probably lost. Reload the page to see where the turn stands.`}
          </div>
        ) : (
          <div className="mvx-admin-muted" style={{ fontSize: 12, marginTop: 4 }}>
            {`Live — the server reported ${Math.floor(heardS)}s ago.`}
          </div>
        )
      )}
    </div>
  );
}

// ── Main AIAssistant component ────────────────────────────────────────────────


// sessionDraftState is what became of a session's draft, for the list: a
// session is its draft (2026-11-13).
function sessionDraftState(s: AISession): string {
  if (s.promoted_at) return "working revision ✓";
  return s.draft_revision_id ? "draft" : "";
}

// sessionProgress is a session's plan and calls, for the list.
function sessionProgress(s: AISession): string {
  const parts: string[] = [];
  if (s.plan_stages) parts.push(`${s.plan_done ?? 0}/${s.plan_stages} stages`);
  if (s.llm_calls) parts.push(`${s.llm_calls} calls`);
  return parts.join(" · ");
}

// formatTokens shortens a token count (1.2M, 340k).
function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`;
  return String(n);
}

// TaskPlanCard shows the plan of a delegated goal, compactly (owner,
// 2026-11-13: the open plan took two screens): one line with the goal and
// progress, the decision waiting and the current stage; expanded, one line
// per stage; a stage's items, evidence and what it took open on a click.
function TaskPlanCard({ plan, onReview }: { plan: AITaskPlan; onReview: () => void }) {
  // A plan waiting for approval is reviewed in the conversation
  // (PlanReviewCard); here it is one line.
  const awaiting = !!plan.pending_approval && plan.stages.length > 0;
  const [open, setOpen] = useState(false);
  const [shown, setShown] = useState<number | null>(null);
  const current = plan.stages.findIndex(s => s.status !== "done" && s.status !== "skipped");
  const done = plan.stages.filter(s => s.status === "done").length;
  const mark = (s: AITaskStage, i: number) =>
    s.status === "done" ? "✓" : s.status === "skipped" ? "–" : i === current ? "▶" : "○";
  const took = (m: NonNullable<AITaskStage["measure"]>) =>
    `${Math.round(m.minutes)} min · ${m.calls} calls · ${formatTokens(m.input_tokens + m.cached_tokens + m.output_tokens)} tokens · ${m.corrections} correction${m.corrections === 1 ? "" : "s"}`;
  const total = plan.stages.reduce((t, s) => s.measure
    ? { minutes: t.minutes + s.measure.minutes, calls: t.calls + s.measure.calls, corrections: t.corrections + s.measure.corrections }
    : t, { minutes: 0, calls: 0, corrections: 0 });
  const openDecisions = (plan.target?.decisions ?? []).filter(d => !d.choice);
  const checks = plan.target?.checks ?? [];
  const recorded = checks.filter(c => c.status && c.status !== "open").length;
  const muted = { marginLeft: 18, fontWeight: 400 } as const;
  const stageLine = (s: AITaskStage, i: number) => (
    <li key={i} style={{ listStyle: "none", fontWeight: i === current ? 600 : 400, color: s.status === "done" || s.status === "skipped" ? "var(--color-text-muted)" : "var(--color-text)" }}>
      <button type="button" onClick={() => setShown(v => (v === i ? null : i))} aria-expanded={shown === i}
        style={{ background: "none", border: 0, padding: 0, cursor: "pointer", textAlign: "left", font: "inherit", fontWeight: "inherit", color: "inherit" }}>
        <span aria-hidden style={{ display: "inline-block", width: 18 }}>{mark(s, i)}</span>
        <Txt>{`${i + 1}. ${s.title}`}</Txt>
      </button>
      {shown === i && (
        <div className="mvx-admin-muted" style={muted}>
          {(s.items?.length ?? 0) > 0 && <div><Txt>{s.items.join("; ")}</Txt></div>}
          {s.evidence && (
            <div><Txt>{[s.evidence.compared && `compared: ${s.evidence.compared}`, s.evidence.differences && s.evidence.differences !== "none" && `differs: ${s.evidence.differences}`]
              .filter(Boolean).join(" · ")}</Txt></div>
          )}
          {s.note && <div><Txt>{s.note}</Txt></div>}
          {s.measure && <div><Txt>{took(s.measure)}</Txt></div>}
        </div>
      )}
    </li>
  );
  if (awaiting) {
    return (
      <div style={{ padding: "8px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12, display: "flex", alignItems: "center", gap: 10 }}>
        <strong style={{ flex: 1 }}><Txt>{`Plan waiting for your approval — ${plan.stages.length} stages: ${plan.goal}`}</Txt></strong>
        <Button size="sm" onClick={onReview}>Review</Button>
      </div>
    );
  }
  return (
    <div style={{ padding: "8px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12, ...(open ? { maxHeight: "60vh", overflowY: "auto" } : {}) }}>
      <button type="button" onClick={() => setOpen(o => !o)} aria-expanded={open} aria-label={open ? "Collapse the plan" : "Expand the plan"}
        title={plan.goal}
        style={{ display: "block", width: "100%", background: "none", border: 0, padding: 0, cursor: "pointer", fontWeight: 600, color: "var(--color-text)",
          textAlign: "left", whiteSpace: "normal", overflowWrap: "anywhere" }}>
        <span aria-hidden style={{ display: "inline-block", width: 14 }}>{open ? "▾" : "▸"}</span>
        <Txt>{`Plan: ${done} of ${plan.stages.length} stages done — ${plan.goal}`}</Txt>
      </button>
      {total.calls > 0 && (
        <span className="mvx-admin-muted"><Txt>{` · ${Math.round(total.minutes)} min, ${total.calls} calls, ${total.corrections} correction${total.corrections === 1 ? "" : "s"} so far`}</Txt></span>
      )}
      {checks.length > 0 && (
        <span className="mvx-admin-muted"><Txt>{` · verification checks: ${recorded} of ${checks.length} recorded`}</Txt></span>
      )}
      {openDecisions.length > 0 && (
        <div role="note" style={{ margin: "6px 0 0", color: "var(--color-warning-text, var(--color-text))" }}>
          <Txt>{`Waiting for your decision: ${openDecisions.map(d => d.options?.length ? `${d.question} (${d.options.join(" / ")})` : d.question).join("; ")}`}</Txt>
        </div>
      )}
      <ol style={{ margin: "6px 0 0", padding: 0 }}>
        {open ? plan.stages.map(stageLine) : current >= 0 && stageLine(plan.stages[current], current)}
      </ol>
      {open && plan.target && (
        <div style={{ marginTop: 10 }}>
          <div style={{ fontWeight: 600, margin: "0 0 6px" }}><Txt>{"Architecture"}</Txt></div>
          <ArchitectureScheme target={plan.target} />
        </div>
      )}
    </div>
  );
}

// DecisionsCard lists what the developer decided in answer to the AI — kept
// with the plan for every later turn and session, and never asked again.
function DecisionsCard({ decisions }: { decisions: string[] }) {
  const [open, setOpen] = useState(false);
  return (
    <div style={{ padding: "8px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12 }}>
      <button type="button" onClick={() => setOpen(o => !o)} aria-expanded={open}
        style={{ background: "none", border: 0, padding: 0, cursor: "pointer", fontWeight: 600, color: "var(--color-text)" }}>
        <Txt>{`Decisions kept: ${decisions.length}`}</Txt>
      </button>
      {open && (
        <ul style={{ margin: "6px 0 0", paddingLeft: 18 }}>
          {decisions.map((d, i) => <li key={i}><Txt>{d}</Txt></li>)}
        </ul>
      )}
    </div>
  );
}

// WorkbookMapCard shows which workbook rows became which metrics, what
// differs, and what is not mapped yet — the AI reads the same map every turn.
function WorkbookMapCard({ sessionId, turns }: { sessionId: string; turns: number }) {
  const [open, setOpen] = useState(false);
  const { data } = useQuery({
    queryKey: ["ai-workbook-map", sessionId, turns],
    queryFn: () => api.aiWorkbookMap(sessionId),
  });
  const sheets = Array.isArray(data?.sheets) ? data.sheets : [];
  if (sheets.length === 0) return null;
  const mapped = sheets.reduce((n, s) => n + s.mapped, 0);
  const match = sheets.reduce((n, s) => n + s.match, 0);
  const differ = sheets.reduce((n, s) => n + s.differ.length, 0);
  const notMapped = sheets.reduce((n, s) => n + s.not_mapped.length, 0);
  return (
    <div style={{ padding: "8px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12 }}>
      <button type="button" onClick={() => setOpen(o => !o)} aria-expanded={open}
        style={{ background: "none", border: 0, padding: 0, cursor: "pointer", fontWeight: 600, color: "var(--color-text)" }}>
        <Txt>{`Workbook map: ${mapped} rows mapped, ${match} match, ${differ} differ, ${notMapped} with numbers not mapped`}</Txt>
      </button>
      {open && (
        <ul style={{ margin: "6px 0 0", paddingLeft: 18 }}>
          {sheets.map(s => (
            <li key={`${s.file}/${s.sheet}`}>
              <Txt>{`${s.sheet} (${s.file}): ${s.mapped} mapped, ${s.match} match`}</Txt>
              {s.differ.length > 0 && <span style={{ color: "var(--color-danger)" }}><Txt>{` — differ: rows ${s.differ.join(", ")}`}</Txt></span>}
              {s.not_mapped.length > 0 && <span className="mvx-admin-muted"><Txt>{` — not mapped: rows ${s.not_mapped.join(", ")}`}</Txt></span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// fmtUSD writes an estimated cost in US dollars: "USD 3.42".
function fmtUSD(n: number): string {
  return `USD ${n.toFixed(2)}`;
}

// CallBudgetControl sets the session's own cap on provider calls: a task
// stops there instead of at the deployment's per-session cap.
function CallBudgetControl({ sessionId, budget, onChange }: { sessionId: string; budget?: number; onChange: (b: number | null) => void }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(budget ? String(budget) : "");
  const save = useMutation({
    mutationFn: (n: number | null) => api.aiSetCallBudget(sessionId, n),
    onSuccess: (r) => { onChange(r.call_budget ?? null); setEditing(false); },
  });
  if (!editing) {
    return (
      <button type="button" className="mvx-admin-muted" onClick={() => { setValue(budget ? String(budget) : ""); setEditing(true); }}
        style={{ background: "none", border: 0, padding: 0, cursor: "pointer", fontSize: "inherit", textDecoration: "underline dotted" }}>
        <Txt>{budget ? `· budget ${budget} calls` : "· set a call budget"}</Txt>
      </button>
    );
  }
  const n = Number.parseInt(value, 10);
  return (
    <span style={{ display: "inline-flex", gap: 4, alignItems: "center" }}>
      <TextInput value={value} onChange={e => setValue(e.target.value)} aria-label="Call budget" placeholder="calls" style={{ width: 70 }} inputMode="numeric" />
      <Button size="sm" disabled={!(n > 0)} loading={save.isPending} onClick={() => save.mutate(n)}>Set</Button>
      {budget != null && <Button size="sm" variant="ghost" onClick={() => save.mutate(null)}>Clear</Button>}
      <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>Cancel</Button>
      {save.error && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
    </span>
  );
}

// fmtTokens writes a token count briefly: 950, 12.4k, 1.25M.
function fmtTokens(n: number): string {
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}k`;
  return String(n);
}
export function AIAssistant({ revisionId, revisionName, onSelectRevision }: {
  revisionId?: string; revisionName?: string;
  /** Makes a revision the console's working revision (the draft, once promoted). */
  onSelectRevision?: (id: string, name: string) => void;
} = {}) {
  const qc = useQueryClient();
  // Developer › Developer Models and Tenant admin › Applications list the
  // revisions a draft adds, becomes or removes: a discarded draft stayed
  // listed there, and deleting it answered "revision not found" (2026-10-29).
  const refreshRevisions = () => {
    for (const key of ["dev-model", "dev-applications", "admin-tenants", "dev-revisions", "dev-revisions-auto"]) {
      qc.invalidateQueries({ queryKey: [key] });
    }
  };
  const [sessionId, setSessionId]   = useState<string | null>(null);
  const [currentSession, setCurrentSession] = useState<AISession | null>(null);
  // What this session's calls were billed for, from the provider's reports.
  const [usage, setUsage] = useState<AISessionUsage | null>(null);
  // The plan of a goal the developer delegated (plan_task); null for none.
  const [taskPlan, setTaskPlan] = useState<AITaskPlan | null>(null);
  // The session's plans no longer under way (one plan per request), and the
  // AI's memory of the session: how full it is and what it holds.
  const [earlierPlans, setEarlierPlans] = useState<AIEarlierPlan[]>([]);
  const [memory, setMemory] = useState<AIMemoryStatus | null>(null);
  const [showMemory, setShowMemory] = useState(false);
  const [compacting, setCompacting] = useState(false);
  const [showAttachPlan, setShowAttachPlan] = useState(false);
  const [approvingPlan, setApprovingPlan] = useState(false);
  const [attachingPlan, setAttachingPlan] = useState(false);
  const [messages, setMessages]     = useState<AIMessage[]>([]);
  const [pendingProposal, setPendingProposal] = useState<AIProposal | null>(null);
  // The open session, for answers that arrive after a switch of sessions.
  const openSession = useRef<string | null>(null);
  const [input, setInput]           = useState("");
  // The table just pasted and attached, until the next message: its note
  // offers it back as plain text.
  const [pasted, setPasted]         = useState<{ name: string; text: string; rows: number; columns: number } | null>(null);
  const [thinking, setThinking]     = useState(false);
  const [streamingContent, setStreamingContent] = useState("");
  const [toolStatus, setToolStatus] = useState<string | null>(null);
  // A long turn shows how long it has run and how much of a proposal the
  // model has written; Stop closes the request, and the server saves
  // "Stopped". proposalWarnings are the plan check's warnings on the
  // proposal shown.
  const [progress, setProgress] = useState<{ tool?: string; chars?: number; steps?: number } | null>(null);
  const [turnStartedAt, setTurnStartedAt] = useState<number | null>(null);
  const [nowTick, setNowTick] = useState(() => Date.now());
  // The step the turn is on, as the server last said (at: when it arrived),
  // and when the server last sent anything: a turn the server stopped
  // telling about is shown as one, not as a timer ticking on its own.
  const [turnStatus, setTurnStatus] = useState<{ s: AITurnStatus; at: number } | null>(null);
  const [heardAt, setHeardAt] = useState(0);
  const [proposalWarnings, setProposalWarnings] = useState<string[]>([]);
  const abortRef = useRef<AbortController | null>(null);
  const [showSettings, setShowSettings] = useState(false);
  const [error, setError]           = useState<string | null>(null);
  const [hoveredSession, setHoveredSession] = useState<string | null>(null);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [documents, setDocuments]   = useState<AIDocument[]>([]);
  // Files already sent with a message: they stay in the session for the
  // assistant to read and import, but leave the composer (they looked
  // attached to the next message — reported live, 2026-10-24).
  const [sentDocIds, setSentDocIds] = useState<Set<string>>(() => new Set());
  const [showSessionFiles, setShowSessionFiles] = useState(false);
  // The session's exports and converted files sit collapsed beside its files
  // (owner, 2026-11-16: always open, they took the space above the message box).
  const [showExports, setShowExports] = useState(false);
  const [showConversions, setShowConversions] = useState(false);
  const sessionExports = useSessionExports(sessionId);
  const { data: sessionConversions = [] } = useQuery<AIConversion[]>({
    queryKey: ["ai-conversions", sessionId],
    queryFn: () => api.aiListConversions(sessionId as string),
    enabled: !!sessionId,
  });
  useEffect(() => { openSession.current = sessionId; }, [sessionId]);
  const [dragOver, setDragOver]     = useState(false);
  const [uploading, setUploading]   = useState(false);
  const [draftBusy, setDraftBusy]   = useState(false);
  // The name the draft takes as it becomes the working revision; null while
  // the banner shows its buttons.
  const [promoteName, setPromoteName] = useState<string | null>(null);
  const [showActivity, setShowActivity] = useState(false);
  const messagesRef = useRef<HTMLDivElement>(null);
  const inputRef  = useRef<HTMLTextAreaElement>(null);
  const fileRef   = useRef<HTMLInputElement>(null);
  const { confirm, confirmElement } = useConfirm();

  // App/model/revision the assistant is scoped to (see banner in chat area).
  const { data: demo } = useDemo(revisionId);
  const { data: sessionModel } = useQuery({
    queryKey: ["dev-model", revisionId ?? ""],
    queryFn: () => api.getDevModel(revisionId || undefined),
  });

  // A session belongs to one model. When the console moves to another
  // model the open session stays behind with it; another revision of the
  // same model (a promoted draft, say) keeps it open.
  const consoleModelId = sessionModel?.model_id;
  const [shownModelId, setShownModelId] = useState(consoleModelId);
  if (consoleModelId && consoleModelId !== shownModelId) {
    setShownModelId(consoleModelId);
    if (currentSession && currentSession.model_id !== consoleModelId) {
      setSessionId(null);
      setCurrentSession(null);
      setMessages([]);
      setDocuments([]);
      setPendingProposal(null);
      setStreamingContent("");
      setToolStatus(null);
      setTurnStatus(null);
    }
  }

  const { data: sessions = [] } = useQuery<AISession[]>({
    queryKey: ["ai-sessions", revisionId ?? ""],
    queryFn: () => api.aiListSessions(revisionId),
  });

  const { data: settings } = useQuery<AISettings>({
    queryKey: ["ai-settings"],
    queryFn: api.aiGetSettings,
  });

  useEffect(() => {
    if (turnStartedAt == null) return;
    const t = setInterval(() => setNowTick(Date.now()), 1000);
    return () => clearInterval(t);
  }, [turnStartedAt]);

  // Auto-scroll to the newest message by scrolling the message list itself:
  // scrollIntoView scrolled every scrollable ancestor too, so on a small
  // screen the console's top bar — which model and revision the assistant
  // works in — left the view right as the revision changed (2026-10-25).
  //
  // The chat follows the AI (owner, 2026-11-13): a new message or plan card
  // brings the view down to it — to the start of the AI's answer when the
  // answer is taller than the view, else to the end — and while a turn
  // streams, the view keeps to the end unless the developer scrolled up to
  // read.
  const atBottom = useRef(true);
  const shownCount = useRef(0);
  useEffect(() => {
    const list = messagesRef.current;
    if (!list) return;
    const grew = messages.length > shownCount.current;
    shownCount.current = messages.length;
    if (!grew && !pendingProposal && !atBottom.current) return;
    const start = list.querySelector<HTMLElement>("[data-turn-start]");
    const top = start && list.scrollHeight - start.offsetTop > list.clientHeight ? start.offsetTop - 12 : list.scrollHeight;
    list.scrollTo({ top, behavior: "smooth" });
  }, [messages, pendingProposal]);
  useEffect(() => {
    const list = messagesRef.current;
    if (list && atBottom.current) list.scrollTo({ top: list.scrollHeight });
  }, [streamingContent, toolStatus, thinking, error, taskPlan]);

  // Load session messages when switching sessions.
  const loadSession = async (id: string) => {
    rememberOpenSession(id);
    setSessionId(id);
    setError(null);
    setPendingProposal(null);
    setStreamingContent("");
    setToolStatus(null);
    setTurnStatus(null);
    const data = await api.aiGetSession(id);
    setCurrentSession(data.session);
    setUsage(data.usage ?? null);
    setTaskPlan(data.task_plan ?? null);
    setEarlierPlans(data.earlier_plans ?? []);
    setMemory(data.memory ?? null);
    setShowAttachPlan(false);
    setMessages(data.messages ?? []);
    setDocuments(data.documents ?? []);
    const lastSent = (data.messages ?? []).filter(m => m.role === "user").at(-1)?.created_at;
    setSentDocIds(new Set((data.documents ?? []).filter(d => lastSent && d.created_at <= lastSent).map(d => d.id)));
    // A plan still waiting for Execute comes back with its session: after a
    // reload or a switch of sessions it was gone from the chat, and the
    // work could not go on (2026-11-13).
    try {
      const pending = (await api.aiListProposals(id)).find(p => p.status === "pending");
      if (pending) {
        setPendingProposal(pending);
        setProposalWarnings(pending.warnings ?? []);
      }
    } catch {
      // The Activity panel still lists it.
    }
  };

  // The session that was open comes back after a reload (it is remembered
  // for this browser tab, and only reopened when it is one of this model's).
  const restored = useRef(false);
  useEffect(() => {
    if (restored.current || sessionId || sessions.length === 0) return;
    let remembered = "";
    try { remembered = sessionStorage.getItem(OPEN_SESSION_KEY) ?? ""; } catch { /* storage unavailable */ }
    if (!remembered || !sessions.some(s => s.id === remembered)) {
      restored.current = true;
      return;
    }
    const t = setTimeout(() => { restored.current = true; void loadSession(remembered); }, 0);
    return () => clearTimeout(t);
  }, [sessions, sessionId]);

  // Create a new session: it continues the developer's open AI draft, or
  // with newDraft makes the next draft on its first confirmed proposal.
  const newSession = useMutation({
    mutationFn: () => api.aiCreateSession(revisionId),
    onSuccess: (sess) => {
      qc.invalidateQueries({ queryKey: ["ai-sessions"] });
      rememberOpenSession(sess.id);
      setSessionId(sess.id);
      setCurrentSession(sess);
      setMessages([]);
      setDocuments([]);
      // A new session starts with no plan and an empty memory; a plan of
      // another session can be attached (optional).
      setTaskPlan(null);
      setEarlierPlans([]);
      setMemory(null);
      setShowAttachPlan(false);
      setPendingProposal(null);
      setStreamingContent("");
      setToolStatus(null);
      setTurnStatus(null);
      setError(null);
    },
    onError: (e) => setError((e as Error).message),
  });

  // Delete a session.
  const renameSession = useMutation({
    mutationFn: ({ id, title }: { id: string; title: string }) => api.aiRenameSession(id, title),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ai-sessions"] }),
  });

  // A deleted session is kept 30 days: listed under the sessions with a
  // Restore, which brings it back and opens it.
  const [showDeleted, setShowDeleted] = useState(false);
  const { data: deletedSessions = [] } = useQuery<AISession[]>({
    queryKey: ["ai-sessions-deleted", revisionId ?? ""],
    queryFn: () => api.aiListDeletedSessions(revisionId),
  });
  const restoreSession = useMutation({
    mutationFn: (id: string) => api.aiRestoreSession(id),
    onSuccess: (sess) => {
      qc.invalidateQueries({ queryKey: ["ai-sessions"] });
      qc.invalidateQueries({ queryKey: ["ai-sessions-deleted"] });
      loadSession(sess.id);
    },
    onError: (e) => setError((e as Error).message),
  });
  const deleteSession = useMutation({
    mutationFn: (id: string) => api.aiDeleteSession(id),
    onSuccess: (_data, id) => {
      qc.invalidateQueries({ queryKey: ["ai-sessions"] });
      qc.invalidateQueries({ queryKey: ["ai-sessions-deleted"] });
      if (sessionId === id) {
        setSessionId(null);
        setCurrentSession(null);
        setMessages([]);
        setDocuments([]);
        setPendingProposal(null);
        setStreamingContent("");
        setToolStatus(null);
        setTurnStatus(null);
      }
    },
  });

  // Take the session's isolated draft as the working revision — not the
  // live one: Set active in Developer › Developer Models is a separate decision. The
  // AI-specific endpoint clears the session's draft_revision_id server-side
  // too, which makes the "AI draft" banner disappear below and finishes the
  // session; the console then switches to the draft.
  // The revision is named as it is taken: a draft otherwise kept "AI Draft"
  // and its time, which the header shows every business user once it goes
  // live. The session's title is offered; blank keeps the draft's name.
  const promoteDraft = async () => {
    if (!sessionId || !currentSession?.draft_revision_id) return;
    setDraftBusy(true);
    setError(null);
    try {
      const resp = await api.aiPromoteDraft(sessionId!, (promoteName ?? "").trim());
      setPromoteName(null);
      setCurrentSession(resp.session);
      setMessages(resp.messages ?? []);
      if (resp.revision) onSelectRevision?.(resp.revision.id, resp.revision.name);
      refreshRevisions();
      qc.invalidateQueries({ queryKey: ["ai-proposals", sessionId] });
    } catch (e) {
      // The name is taken (a session promoted before goes on and offers its
      // title again): the server's next free name goes in the box.
      const body = (e as { body?: { code?: string; suggested_name?: string } }).body;
      if (body?.code === "REVISION_NAME_TAKEN" && body.suggested_name) {
        setError(`A revision is already named “${(promoteName ?? "").trim()}” — the name is now “${body.suggested_name}”; press Use as working revision again, or change it.`);
        setPromoteName(body.suggested_name);
      } else {
        setError((e as Error).message);
      }
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
          refreshRevisions();
          qc.invalidateQueries({ queryKey: ["ai-proposals", sessionId] });
        } catch (e) {
          setError((e as Error).message);
        } finally {
          setDraftBusy(false);
        }
      },
    });
  };

  // Upload documents into the current session, one after another (creates
  // a session if needed — once, for all of them).
  const uploadDocuments = async (files: File[]) => {
    if (files.length === 0) return;
    setUploading(true);
    setError(null);
    try {
      let sid = sessionId;
      if (!sid) {
        const sess = await api.aiCreateSession(revisionId);
        qc.invalidateQueries({ queryKey: ["ai-sessions"] });
        sid = sess.id;
        rememberOpenSession(sid);
        setSessionId(sid);
        setCurrentSession(sess);
        setMessages([]);
      }
      for (const file of files) {
        const doc = await api.aiUploadDocument(sid, file);
        setDocuments(prev => [...prev, doc]);
      }
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setUploading(false);
      if (fileRef.current) fileRef.current.value = "";
    }
  };

  // A table pasted from a spreadsheet is attached as a CSV, so it imports like
  // any file instead of the assistant retyping it value by value.
  const onPasteMessage = (e: ClipboardEvent<HTMLTextAreaElement>) => {
    const text = e.clipboardData.getData("text/plain");
    const table = parsePastedTable(text);
    if (!table || thinking || uploading) return;
    e.preventDefault();
    const taken = new Set(documents.map(d => d.filename));
    let n = 1;
    while (taken.has(`pasted-data-${n}.csv`)) n++;
    const name = `pasted-data-${n}.csv`;
    void uploadDocuments([new File([table.csv], name, { type: "text/csv" })]);
    setPasted({ name, text, rows: table.rows, columns: table.columns });
  };

  const pastedBackToText = async () => {
    if (!pasted) return;
    const doc = documents.find(d => d.filename === pasted.name);
    if (doc) await removeDocument(doc.id);
    setInput(prev => prev + pasted.text);
    setPasted(null);
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

  // Auto-continue: when a confirmed proposal executes ALL 100 steps (the
  // server-side batch cap), the plan almost certainly has more batches —
  // send the follow-up automatically instead of making the developer type
  // "continue" between every batch. Each batch still lands as a proposal
  // needing explicit confirmation; this only removes the typing. Capped so
  // a model that keeps emitting full batches can't loop forever; any manual
  // message resets the budget.

  // Send a message.
  const sendMessage = async () => {
    const content = input.trim();
    if (!content || thinking) return;

    // Ensure we have a session.
    let sid = sessionId;
    if (!sid) {
      if (!settings?.has_key && !settings?.tenant_key && !import.meta.env.VITE_OPENAI_API_KEY) {
        setShowSettings(true);
        setError("Set your API key in Settings before chatting.");
        return;
      }
      try {
        const sess = await api.aiCreateSession(revisionId);
        qc.invalidateQueries({ queryKey: ["ai-sessions"] });
        sid = sess.id;
        setSessionId(sid);
        setCurrentSession(sess);
      } catch (e) {
        setError((e as Error).message);
        return;
      }
    }

    setInput("");
    setPasted(null);
    setSentDocIds(new Set(documents.map(d => d.id)));
    await sendContent(sid, content);
  };

  // sendContent runs one chat turn (streaming) for an existing session —
  // shared by manual sends and the turn that goes on by itself after a
  // confirmed plan ran (goOn: the server writes that turn's message).
  const sendContent = async (sid: string, content: string, goOn = false) => {
    // Optimistically add the user message.
    const optimistic: AIMessage = {
      id: `opt-${Date.now()}`,
      session_id: sid,
      role: "user",
      content: goOn ? `${CONTINUE_PREFIX}Plan confirmed — going on` : content,
      created_at: new Date().toISOString(),
    };
    setMessages(prev => [...prev, optimistic]);
    setThinking(true);
    setError(null);

    setStreamingContent("");
    setToolStatus(null);
    setTurnStatus(null);
    setProgress(null);
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    setTurnStartedAt(Date.now());
    setNowTick(Date.now());
    setHeardAt(Date.now());

    try {
      await api.aiSendMessage(sid, content, (event) => {
        setHeardAt(Date.now());
        switch (event.type) {
          case "delta":
            setStreamingContent(prev => prev + (event.content ?? ""));
            break;
          case "tool_status":
            setToolStatus(event.tool ?? null);
            setProgress(null);
            setStreamingContent(""); // a server step is starting
            break;
          case "status": {
            const s = event as AITurnStatus;
            // A new call or a server step: the last call's writing is over.
            if (s.phase !== "ai" || s.stage === "sending") {
              setProgress(null);
              setStreamingContent("");
            }
            setTurnStatus({ s, at: Date.now() });
            break;
          }
          case "progress":
            setProgress({ tool: event.tool, chars: event.chars, steps: event.steps });
            break;
          case "proposal":
            setStreamingContent("");
            setToolStatus(null);
            setTurnStatus(null);
            setProgress(null);
            setMessages(event.messages ?? []);
            setPendingProposal(event.proposal ?? null);
            setProposalWarnings(event.warnings ?? []);
            if (event.session) setCurrentSession(event.session);
            if (event.usage) setUsage(event.usage);
            if (event.memory) setMemory(event.memory);
            qc.invalidateQueries({ queryKey: ["ai-proposals", sid] });
            break;
          case "task_plan":
            setTaskPlan(event.task_plan ?? null);
            break;
          case "memory":
            if (event.memory) setMemory(event.memory);
            // A note saved before the reply (an attachment gone through, the
            // memory compacted) shows once the turn ends.
            break;
          case "done":
            setStreamingContent("");
            setToolStatus(null);
            setTurnStatus(null);
            setMessages(event.messages ?? []);
            // A turn that ends without a new plan leaves the one waiting for
            // Execute: it is asked for again, as a reload does. Cleared here,
            // it vanished at the next message, and the AI went on asking for
            // an Execute no card offered (2026-11-16).
            void api.aiListProposals(sid).then(list => {
              if (openSession.current !== sid) return;
              const waiting = list.find(p => p.status === "pending") ?? null;
              setPendingProposal(waiting);
              setProposalWarnings(waiting?.warnings ?? []);
            }).catch(() => {
              if (openSession.current === sid) setPendingProposal(null);
            });
            if (event.session) setCurrentSession(event.session);
            if (event.usage) setUsage(event.usage);
            if (event.task_plan !== undefined) setTaskPlan(event.task_plan ?? null);
            if (event.earlier_plans) setEarlierPlans(event.earlier_plans);
            if (event.memory) setMemory(event.memory);
            break;
          case "error":
            setError(event.error ?? "Something went wrong");
            setMessages(prev => prev.filter(m => m.id !== optimistic.id));
            break;
        }
      }, revisionId, ctrl.signal, goOn);
      // The auto-generated title lands asynchronously shortly after the
      // first send — refetch the list now and once more after the namer's
      // window so the "Session" placeholder becomes the real name.
      qc.invalidateQueries({ queryKey: ["ai-sessions"] });
      qc.invalidateQueries({ queryKey: ["ai-conversions", sid] });
      setTimeout(() => qc.invalidateQueries({ queryKey: ["ai-sessions"] }), 5000);
    } catch (e) {
      if (ctrl.signal.aborted) {
        // Stopped: the server saved what it had and "Stopped by the developer".
        setTimeout(() => {
          api.aiGetSession(sid).then(data => setMessages(data.messages ?? [])).catch(() => undefined);
        }, 800);
      } else {
        setError((e as Error).message);
        // Remove optimistic message on failure.
        setMessages(prev => prev.filter(m => m.id !== optimistic.id));
      }
    } finally {
      abortRef.current = null;
      setTurnStartedAt(null);
      setProgress(null);
      setThinking(false);
      setStreamingContent("");
      setToolStatus(null);
      setTurnStatus(null);
      inputRef.current?.focus();
    }
  };
  const stopTurn = () => abortRef.current?.abort();
  const elapsed = turnStartedAt == null ? "" : formatElapsed(nowTick - turnStartedAt);

  // Compacts the memory now, at the developer's request.
  const compactMemory = async () => {
    if (!sessionId) return;
    setCompacting(true);
    setError(null);
    try {
      const resp = await api.aiCompactMemory(sessionId);
      setMemory(resp.memory);
      setMessages(resp.messages ?? []);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setCompacting(false);
    }
  };
  // Approves the plan that waits; the AI then builds its first stage by
  // itself (owner, 2026-11-13).
  const approvePlan = async () => {
    if (!sessionId) return;
    setApprovingPlan(true);
    setError(null);
    try {
      const resp = await api.aiApprovePlan(sessionId);
      setTaskPlan(resp.task_plan);
      void sendContent(sessionId, "", true);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setApprovingPlan(false);
    }
  };
  // Attaches a plan of another session (optional, never automatic).
  const attachPlan = async (ref: { session_id: string; index: number }, attachments: boolean) => {
    if (!sessionId) return;
    setAttachingPlan(true);
    setError(null);
    try {
      const resp = await api.aiAttachPlan(sessionId, { session_id: ref.session_id, index: ref.index, attachments });
      setTaskPlan(resp.task_plan);
      setEarlierPlans(resp.earlier_plans ?? []);
      setMessages(resp.messages ?? []);
      setDocuments(resp.documents ?? []);
      setMemory(resp.memory);
      setShowAttachPlan(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setAttachingPlan(false);
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
          title="A new session is its own draft: its first confirmed plan copies your working revision"
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
                <div style={{ whiteSpace: "normal", overflowWrap: "anywhere" }}>
                  {s.title || "Session"}
                </div>
                <div style={{ fontSize: 10, color: "var(--color-text-subtle)", marginTop: 1 }}>
                  {[s.created_at.slice(0, 10), sessionDraftState(s), sessionProgress(s)].filter(Boolean).join(" · ")}
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
                  onClick={(e) => {
                    e.stopPropagation();
                    confirm({
                      title: "Delete this session?",
                      body: `"${s.title || "Session"}" goes from the list with its chat, plan and memory. It is kept for 30 days: restore it from "Deleted sessions" below the list. The model and its revisions are not touched.`,
                      confirmLabel: "Delete session",
                      destructive: true,
                      onConfirm: () => deleteSession.mutate(s.id),
                    });
                  }}
                >
                  <Trash2 size={12} />
                </IconButton>
              </>
            )}
          </div>
        ))}

        {deletedSessions.length > 0 && (
          <div style={{ marginTop: 8 }}>
            <button
              type="button"
              className="mvx-admin-muted"
              style={{ background: "none", border: "none", padding: "2px 4px", fontSize: 11, cursor: "pointer", display: "flex", alignItems: "center", gap: 4 }}
              aria-expanded={showDeleted}
              onClick={() => setShowDeleted(v => !v)}
            >
              <Trash2 size={11} /> {`Deleted sessions (${deletedSessions.length})`}
            </button>
            {showDeleted && deletedSessions.map(s => (
              <div key={s.id} style={{ display: "flex", alignItems: "center", gap: 4, padding: "4px 4px 4px 8px" }}>
                <div style={{ minWidth: 0, flex: 1 }}>
                  <div style={{ fontSize: 12, whiteSpace: "normal", overflowWrap: "anywhere" }}>{s.title || "Session"}</div>
                  <div style={{ fontSize: 10, color: "var(--color-text-subtle)" }}>
                    {`deleted ${s.deleted_at ? new Date(s.deleted_at).toLocaleString([], { dateStyle: "short", timeStyle: "short" }) : ""}`}
                  </div>
                </div>
                <Button size="sm" variant="ghost" disabled={restoreSession.isPending} onClick={() => restoreSession.mutate(s.id)}
                  title="Bring the session back with its chat, plan and memory">
                  Restore
                </Button>
              </div>
            ))}
          </div>
        )}

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
            {/* The draft's two actions sit on this line: the banner they had
                stayed on screen for the whole session (owner, 2026-11-13). */}
            {currentSession?.draft_revision_id && (
              <span title="Every plan of this session goes into its AI draft until you use it as your working revision or discard it. It goes live only when you Set active in Developer › Developer Models; a new session starts its own draft.">
                · writes go to your AI draft:
              </span>
            )}
            {currentSession?.draft_revision_id && promoteName === null && (
              <>
                <button type="button" className="mvx-link-button" disabled={draftBusy}
                  onClick={() => setPromoteName((currentSession.title ?? "").trim().slice(0, 80))}>use as working revision</button>
                <button type="button" className="mvx-link-button" disabled={draftBusy} onClick={discardDraft}>discard</button>
              </>
            )}
            {usage && usage.llm_calls > 0 && (
              <span title={`Input ${usage.input_tokens.toLocaleString()}, read from cache ${usage.cache_read_tokens.toLocaleString()}, written to cache ${usage.cache_write_tokens.toLocaleString()}, output ${usage.output_tokens.toLocaleString()}`}>
                <Txt>{`· ${usage.llm_calls}${currentSession?.call_budget ? ` of ${currentSession.call_budget}` : ""} calls, ${fmtTokens(usage.input_tokens + usage.cache_read_tokens + usage.cache_write_tokens)} tokens in, ${fmtTokens(usage.output_tokens)} out`}</Txt>
                {usage.estimated_cost_usd != null && <Txt>{` · about ${fmtUSD(usage.estimated_cost_usd)}`}</Txt>}
              </span>
            )}
            {sessionId && (
              <CallBudgetControl sessionId={sessionId} budget={currentSession?.call_budget}
                onChange={b => setCurrentSession(s => (s ? { ...s, call_budget: b ?? undefined } : s))} />
            )}
            {memory && <MemoryGauge memory={memory} open={showMemory} onToggle={() => setShowMemory(v => !v)} />}
            <span aria-hidden>·</span>
            <button type="button" className="mvx-link-button" aria-expanded={showAttachPlan} disabled={thinking}
              title="Optional: go on in this session with a plan of another of your sessions"
              onClick={() => setShowAttachPlan(v => !v)}>
              Attach a plan…
            </button>
          </div>
        )}
        {/* Naming the draft as it becomes the working revision: shown only
            while the developer does that. */}
        {currentSession?.draft_revision_id && promoteName !== null && (
          <div style={{ display: "flex", alignItems: "center", gap: 10, padding: "8px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12 }}>
            <span style={{ flex: 1 }}><Txt>{"Name the revision your AI draft becomes (it goes live only when you Set active in Developer Models):"}</Txt></span>
            <TextInput
              aria-label="Name of the revision"
              value={promoteName}
              onChange={(e) => setPromoteName(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") promoteDraft(); if (e.key === "Escape") setPromoteName(null); }}
              placeholder="Revision name (blank keeps the draft's)"
              autoFocus
              style={{ width: 240 }}
            />
            <Button size="sm" variant="primary" loading={draftBusy} loadingLabel="Working…" onClick={promoteDraft}>
              Use as working revision
            </Button>
            <Button size="sm" disabled={draftBusy} onClick={() => setPromoteName(null)}>
              Cancel
            </Button>
          </div>
        )}

        {sessionId && showMemory && memory && <MemoryPanel memory={memory} busy={compacting} onCompact={() => { void compactMemory(); }} />}
        {sessionId && showAttachPlan && (
          <AttachPlanPanel sessionId={sessionId} hasPlan={(taskPlan?.stages?.length ?? 0) > 0} busy={attachingPlan}
            onAttach={(ref, files) => { void attachPlan(ref, files); }} onClose={() => setShowAttachPlan(false)} />
        )}

        {taskPlan && (taskPlan.stages?.length ?? 0) > 0 && (
          <TaskPlanCard plan={taskPlan} onReview={() => {
            const list = messagesRef.current;
            const card = list?.querySelector<HTMLElement>("[data-plan-review]");
            if (list && card) list.scrollTo({ top: card.offsetTop - 12, behavior: "smooth" });
          }} />
        )}
        {taskPlan && (taskPlan.decisions?.length ?? 0) > 0 && <DecisionsCard decisions={taskPlan.decisions ?? []} />}
        {earlierPlans.length > 0 && <EarlierPlansCard plans={earlierPlans} />}
        {sessionId && <WorkbookMapCard sessionId={sessionId} turns={messages.length} />}

        {showActivity && sessionId && (
          <ActivityPanel sessionId={sessionId} onClose={() => setShowActivity(false)} />
        )}

        {/* Draft revision banner — shown while the session works in the
            developer's AI draft (made by a first confirmed proposal, continued
            by every new session). AI writes always land here, never in an
            existing revision; promoting makes it the working revision, never
            the live one. */}
        {/* Messages */}
        <div ref={messagesRef} style={{ flex: 1, minHeight: 0, overflowY: "auto", padding: "20px 24px", position: "relative" }}
          onScroll={e => {
            const el = e.currentTarget;
            atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 160;
          }}>
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

          <MessageList messages={messages} />

          {/* The plan waiting for approval: reviewed and approved here. */}
          {taskPlan?.pending_approval && (taskPlan.stages?.length ?? 0) > 0 && sessionId && (
            <PlanReviewCard plan={taskPlan} approving={approvingPlan} onApprove={thinking ? undefined : () => { void approvePlan(); }}
              onChange={() => inputRef.current?.focus()} />
          )}

          {/* Pending proposal — show after the last assistant message */}
          {pendingProposal && sessionId && proposalWarnings.length > 0 && (
            <div className="mvx-chat-row">
              <div className="mvx-chat-bubble" role="note" style={{ border: "1px solid var(--color-warning-border)", background: "var(--color-warning-bg)", color: "var(--color-warning-text)" }}>
                <strong>The plan check warns:</strong>
                <ul style={{ margin: "4px 0 0", paddingLeft: 18 }}>
                  {proposalWarnings.map((w, i) => <li key={i}>{w}</li>)}
                </ul>
              </div>
            </div>
          )}
          {pendingProposal && sessionId && (
            <ProposalPanel
              proposal={pendingProposal}
              sessionId={sessionId}
              revisionId={revisionId}
              onSettled={(msgs, updated, session) => {
                setMessages(msgs);
                setPendingProposal(updated.status === "pending" ? updated : null);
                if (session) setCurrentSession(session);
                // A confirmed plan's first step makes the session's draft.
                if (session && session.draft_revision_id !== currentSession?.draft_revision_id) refreshRevisions();
                qc.invalidateQueries({ queryKey: ["ai-proposals", sessionId] });
                // A confirmed plan that ran: the AI goes on with the next step
                // by itself (owner, 2026-11-13) — fixing failed steps first.
                // Every plan it proposes still waits for Execute.
                if (updated.status === "executed" || updated.status === "partial") {
                  void sendContent(sessionId, "", true);
                }
              }}
            />
          )}

          {thinking && streamingContent && (
            <div className="mvx-chat-row">
              <div className="mvx-chat-bubble">{streamingContent}{elapsed && <span className="mvx-admin-muted" style={{ marginLeft: 8 }}>{elapsed}</span>}</div>
            </div>
          )}

          {thinking && !streamingContent && (
            <div className="mvx-chat-row">
              <TurnStatusBubble progress={progress} toolStatus={toolStatus} status={turnStatus}
                heardAt={heardAt} now={nowTick} elapsed={elapsed} />
            </div>
          )}

          {error && <p className="mvx-admin-error" style={{ marginBottom: 8 }}>{error}</p>}

        </div>

        {/* Input bar — files can be dropped on it as well as attached. */}
        <div
          style={{ padding: "12px 24px 20px", borderTop: "1px solid var(--color-border)",
            outline: dragOver ? "2px dashed var(--color-brand-500)" : undefined, outlineOffset: -6 }}
          onDragOver={e => {
            if (!e.dataTransfer.types.includes("Files") || thinking) return;
            e.preventDefault();
            e.dataTransfer.dropEffect = "copy";
            setDragOver(true);
          }}
          onDragLeave={e => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragOver(false); }}
          onDrop={e => {
            if (!e.dataTransfer.types.includes("Files")) return;
            e.preventDefault();
            setDragOver(false);
            if (thinking || uploading) return;
            const files = Array.from(e.dataTransfer.files);
            const accepted = files.filter(f => ATTACHABLE.split(",").some(ext => f.name.toLowerCase().endsWith(ext)));
            const refused = files.filter(f => !accepted.includes(f)).map(f => f.name);
            void uploadDocuments(accepted).then(() => {
              if (refused.length === 0) return;
              const note = `Not attached: ${refused.join(", ")} — attach pdf, xlsx, xlsm, docx, csv, txt, md, json or log files.`;
              setError(prev => (prev ? `${prev} ${note}` : note));
            });
          }}
        >
          {/* Files sent earlier, the session's exports and its converted
              files stay in the session, out of the way: one line of links. */}
          {(documents.some(d => sentDocIds.has(d.id)) || sessionExports.length > 0 || sessionConversions.length > 0) && (
            <div className="mvx-admin-muted" style={{ fontSize: 12, marginBottom: 6, display: "flex", flexWrap: "wrap", gap: "2px 16px" }}>
              {documents.some(d => sentDocIds.has(d.id)) && (
                <span>
                  <button type="button" className="mvx-link-button" aria-expanded={showSessionFiles} onClick={() => setShowSessionFiles(v => !v)}>
                    {showSessionFiles ? "Hide" : "Show"} the session's files ({documents.filter(d => sentDocIds.has(d.id)).length})
                  </button>
                  <Txt>{" — the assistant can still read and import them."}</Txt>
                </span>
              )}
              {sessionExports.length > 0 && (
                <button type="button" className="mvx-link-button" aria-expanded={showExports} onClick={() => setShowExports(v => !v)}>
                  {showExports ? "Hide" : "Show"} exports ({sessionExports.length})
                </button>
              )}
              {sessionConversions.length > 0 && (
                <button type="button" className="mvx-link-button" aria-expanded={showConversions} onClick={() => setShowConversions(v => !v)}>
                  {showConversions ? "Hide" : "Show"} converted files ({sessionConversions.length})
                </button>
              )}
            </div>
          )}
          {showExports && sessionExports.length > 0 && <SessionExports exports={sessionExports} />}
          {showConversions && sessionId && sessionConversions.length > 0 && <SessionConversions sessionId={sessionId} conversions={sessionConversions} />}
          {/* Attached document chips: files for the next message, and the
              session's earlier files when shown. */}
          {(documents.some(d => showSessionFiles || !sentDocIds.has(d.id)) || uploading) && (
            <div style={{ display: "flex", flexWrap: "wrap", gap: 6, marginBottom: 8 }}>
              {documents.filter(d => showSessionFiles || !sentDocIds.has(d.id)).map(d => (
                <FilterChip
                  key={d.id}
                  title={d.importable
                    ? "Spreadsheet kept whole — the assistant can import all of it into a grid or dimension"
                    : `${d.char_count.toLocaleString()} chars extracted${d.truncated ? " (truncated)" : ""}`}
                  onClear={() => removeDocument(d.id)}
                >
                  {d.importable ? <FileSpreadsheet size={11} aria-hidden="true" /> : <FileText size={11} aria-hidden="true" />}<Txt>{" "}</Txt>
                  {d.filename}<Txt>{d.truncated && !d.importable ? " ⚠" : ""}</Txt>
                </FilterChip>
              ))}
              {uploading && (
                <span className="mvx-admin-muted" style={{ padding: "3px 4px" }}>Uploading…</span>
              )}
            </div>
          )}
          {pasted && (
            <div role="status" style={{ display: "flex", gap: 10, alignItems: "center", marginBottom: 8 }}>
              <span className="mvx-admin-muted" style={{ flex: 1 }}>
                Pasted table attached as {pasted.name}: {pasted.rows} rows, {pasted.columns} columns. Say where it goes.
              </span>
              <Button size="sm" variant="secondary" onClick={() => { void pastedBackToText(); }}>Use as text</Button>
            </div>
          )}
          <div style={{ display: "flex", gap: 10, alignItems: "flex-end" }}>
            <input
              ref={fileRef}
              type="file"
              multiple
              accept={ATTACHABLE}
              style={{ display: "none" }}
              onChange={e => { void uploadDocuments(Array.from(e.target.files ?? [])); }}
            />
            <IconButton
              aria-label="Attach documents"
              title="Attach documents (pdf, xlsx, docx, csv, txt, md) — or drop them here"
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
              onPaste={onPasteMessage}
              placeholder={sessionId ? "Ask a question or say what to build… (Enter to send, Shift+Enter for a new line)" : "Start a new session to begin chatting"}
              disabled={thinking}
              rows={2}
              style={{ flex: 1, resize: "none" }}
              aria-label="Message"
            />
            {thinking ? (
              <Button
                variant="dangerSecondary"
                onClick={stopTurn}
                style={{ flexShrink: 0, height: 42 }}
                aria-label="Stop the turn"
              >
                Stop
              </Button>
            ) : (
              <Button
                variant="primary"
                disabled={!input.trim()}
                onClick={sendMessage}
                style={{ flexShrink: 0, height: 42 }}
              >
                Send
              </Button>
            )}
          </div>
          <div className="mvx-admin-muted" style={{ marginTop: 6, fontSize: 11 }}>
            Provider: {settings?.provider || "openai"} · Model: {settings?.model || settings?.default_models?.[settings?.provider || "openai"] || "provider default"}
          </div>
        </div>
      </div>
      {confirmElement}
    </div>
  );
}
