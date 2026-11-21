import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type AIEarlierPlan, type AIMemoryStatus, type AIPlanRef } from "../../api/client";
import { Button, Txt } from "../../ui";

// The AI Developer's memory of a session and its plans (owner, 2026-11-13):
// the developer sees how full the memory is and what it holds, can compact
// it, can attach a plan of another session (never a must), and sees the
// session's earlier plans — one plan per request.

function fmtK(n: number): string {
  return n >= 1000 ? `${Math.round(n / 1000)}k` : String(n);
}

function when(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return d.toLocaleString(undefined, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}

/** The memory's fill as a small bar with its percentage; a click opens what it holds. */
export function MemoryGauge({ memory, open, onToggle }: { memory: AIMemoryStatus; open: boolean; onToggle: () => void }) {
  const pct = Math.min(memory.percent, 100);
  const tone = memory.percent >= memory.compact_at_percent ? "var(--color-danger-500, #d14343)"
    : memory.percent >= 60 ? "var(--color-warning-500, #d99a00)" : "var(--color-brand-500)";
  return (
    <button type="button" onClick={onToggle} aria-expanded={open}
      title={`AI memory of this session: about ${fmtK(memory.used_tokens)} of ${fmtK(memory.capacity_tokens)} tokens. Compacted automatically at ${memory.compact_at_percent}%.`}
      style={{ display: "inline-flex", alignItems: "center", gap: 6, background: "none", border: 0, padding: 0, cursor: "pointer", font: "inherit", color: "inherit" }}>
      <Txt>{"· memory"}</Txt>
      <span aria-hidden style={{ display: "inline-block", width: 48, height: 6, borderRadius: 3, background: "var(--color-border)", overflow: "hidden" }}>
        <span style={{ display: "block", width: `${pct}%`, height: "100%", background: tone }} />
      </span>
      <strong style={{ color: "var(--color-text)" }}><Txt>{`${memory.percent}%`}</Txt></strong>
    </button>
  );
}

/** What the memory holds, and Compact now. */
export function MemoryPanel({ memory, busy, onCompact }: { memory: AIMemoryStatus; busy: boolean; onCompact: () => void }) {
  const [showSummary, setShowSummary] = useState(false);
  const row = { margin: "4px 0 0" } as const;
  return (
    <div style={{ padding: "8px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <strong style={{ flex: 1 }}>
          <Txt>{`Memory: ${memory.percent}% full — about ${fmtK(memory.used_tokens)} of ${fmtK(memory.capacity_tokens)} tokens`}</Txt>
        </strong>
        <Button size="sm" variant="secondary" loading={busy} loadingLabel="Compacting…" onClick={onCompact}
          title="Turn the earlier conversation into a summary the AI keeps; the plan, decisions, attachment analyses and notes stay whole">
          Compact now
        </Button>
      </div>
      <div className="mvx-admin-muted" style={row}>
        <Txt>{`Conversation ${fmtK(memory.conversation_tokens)} · attachment analyses ${fmtK(memory.attachment_tokens)} · notes ${fmtK(memory.notes_tokens)} · summary ${fmtK(memory.summary_tokens)}. `}</Txt>
        <Txt>{`Compacted automatically at ${memory.compact_at_percent}%`}</Txt>
        <Txt>{memory.compactions > 0 ? ` — ${memory.compactions} time${memory.compactions === 1 ? "" : "s"} so far, last on ${when(memory.compacted_at)}.` : "; not compacted yet."}</Txt>
      </div>
      {memory.attachments.length > 0 && (
        <div style={row}>
          <Txt>{"Attachments gone through once (the AI works from these analyses, and reads a file again only when you ask): "}</Txt>
          <Txt>{memory.attachments.map(a => `${a.file} (${a.by === "server" ? "read by the server" : "analysed by the AI"}, ${when(a.at)}, ${fmtK(a.tokens)} tokens)`).join("; ")}</Txt>
        </div>
      )}
      {memory.reads > 0 && (
        <div className="mvx-admin-muted" style={row}>
          <Txt>{`${memory.reads} part${memory.reads === 1 ? "" : "s"} of attachments read and kept — asked again, they come from memory.`}</Txt>
        </div>
      )}
      {memory.notes.length > 0 && (
        <div style={row}>
          <Txt>{"Notes the AI keeps:"}</Txt>
          <ul style={{ margin: "2px 0 0", paddingLeft: 18 }}>
            {memory.notes.map((n, i) => <li key={i}><Txt>{n}</Txt></li>)}
          </ul>
        </div>
      )}
      {memory.summary && (
        <div style={row}>
          <button type="button" className="mvx-link-button" aria-expanded={showSummary} onClick={() => setShowSummary(v => !v)}>
            <Txt>{showSummary ? "Hide the summary of the earlier conversation" : "Show the summary of the earlier conversation"}</Txt>
          </button>
          {showSummary && <div style={{ whiteSpace: "pre-wrap", marginTop: 4 }}><Txt>{memory.summary}</Txt></div>}
        </div>
      )}
    </div>
  );
}

/** The developer's plans in other sessions of the model; attaching one is optional. */
export function AttachPlanPanel({ sessionId, hasPlan, busy, onAttach, onClose }: {
  sessionId: string; hasPlan: boolean; busy: boolean;
  onAttach: (ref: AIPlanRef, attachments: boolean) => void; onClose: () => void;
}) {
  const [withFiles, setWithFiles] = useState(true);
  const { data: plans, isLoading, error } = useQuery({
    queryKey: ["ai-plans", sessionId],
    queryFn: () => api.aiListPlans(sessionId),
  });
  const stateLabel = (p: AIPlanRef) => (p.state === "current" ? "that session's plan" : p.state === "finished" ? "finished" : "set aside");
  return (
    <div style={{ padding: "8px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <strong style={{ flex: 1 }}>Attach a plan from another session</strong>
        <Button size="sm" variant="ghost" onClick={onClose}>Close</Button>
      </div>
      <div className="mvx-admin-muted" style={{ margin: "4px 0 6px" }}>
        <Txt>{"Optional: this session goes on with the plan you pick."}</Txt>
        <Txt>{hasPlan ? " The plan under way here is set aside in this session, not lost." : ""}</Txt>
      </div>
      <label style={{ display: "flex", gap: 6, alignItems: "center", marginBottom: 6 }}>
        <input type="checkbox" checked={withFiles} onChange={e => setWithFiles(e.target.checked)} />
        <Txt>{"Bring its attachments along with their analysis (not gone through again)"}</Txt>
      </label>
      {isLoading && <div className="mvx-admin-muted">Loading plans…</div>}
      {error && <div role="alert"><Txt>{(error as Error).message}</Txt></div>}
      {plans && plans.length === 0 && <div className="mvx-admin-muted">No other session of yours in this model has a plan.</div>}
      {plans && plans.length > 0 && (
        <ul style={{ margin: 0, padding: 0, listStyle: "none", maxHeight: 220, overflowY: "auto" }}>
          {plans.map(p => (
            <li key={`${p.session_id}:${p.index}`} style={{ display: "flex", gap: 10, alignItems: "center", padding: "4px 0", borderTop: "1px solid var(--color-border)" }}>
              <span style={{ flex: 1, minWidth: 0 }} title={p.goal}>
                <strong style={{ overflowWrap: "anywhere" }}>
                  <Txt>{p.goal || "(no goal)"}</Txt>
                </strong>
                <span className="mvx-admin-muted">
                  <Txt>{` — ${p.done} of ${p.stages} stages done, ${stateLabel(p)} · session “${p.session_title || when(p.session_created_at)}”`}</Txt>
                  <Txt>{p.attachments.length > 0 ? ` · files: ${p.attachments.join(", ")}` : ""}</Txt>
                </span>
              </span>
              <Button size="sm" variant="secondary" disabled={busy} onClick={() => onAttach(p, withFiles)}>Attach</Button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** The session's plans no longer under way (one plan per request). */
export function EarlierPlansCard({ plans }: { plans: AIEarlierPlan[] }) {
  const [open, setOpen] = useState(false);
  const done = (p: AIEarlierPlan) => p.plan.stages.filter(s => s.status === "done" || s.status === "skipped").length;
  return (
    <div style={{ padding: "6px 24px", borderBottom: "1px solid var(--color-border)", fontSize: 12 }}>
      <button type="button" onClick={() => setOpen(o => !o)} aria-expanded={open}
        style={{ background: "none", border: 0, padding: 0, cursor: "pointer", fontWeight: 600, color: "var(--color-text)" }}>
        <span aria-hidden style={{ display: "inline-block", width: 14 }}>{open ? "▾" : "▸"}</span>
        <Txt>{`Earlier plans in this session: ${plans.length}`}</Txt>
      </button>
      {open && (
        <ol style={{ margin: "4px 0 0", paddingLeft: 32 }}>
          {plans.map((p, i) => (
            <li key={i}>
              <Txt>{`${p.plan.goal} — ${done(p)} of ${p.plan.stages.length} stages done, ${p.state === "finished" ? "finished" : "set aside"} ${when(p.at)}`}</Txt>
            </li>
          ))}
        </ol>
      )}
      {open && <div className="mvx-admin-muted" style={{ marginTop: 4 }}><Txt>{"Ask the AI to take one up again by its number."}</Txt></div>}
    </div>
  );
}
