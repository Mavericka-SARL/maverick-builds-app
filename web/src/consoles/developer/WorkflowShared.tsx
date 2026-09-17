import { CheckSquare, BadgeCheck, GitFork, Bell, Merge } from "lucide-react";
import { IconButton, Dialog, Field as UiField, StatusBadge as UiStatusBadge } from "../../ui";
import type { StepType, TriggerEventCatalogItem } from "../../api/client";
import {
  STATUS_TONES, TRIGGER_FALLBACK, TRIGGER_CATEGORY_ORDER, TRIGGER_CATEGORY_LABELS,
  useTriggerEvents, inputStyle,
} from "./workflowConstants";

export function StepTypeIcon({ type, size = 14 }: { type: StepType; size?: number }) {
  const props = { size, "aria-hidden": true as const, style: { flexShrink: 0, verticalAlign: -2 } };
  switch (type) {
    case "approval":     return <BadgeCheck {...props} />;
    case "condition":    return <GitFork {...props} />;
    case "notification": return <Bell {...props} />;
    case "join":         return <Merge {...props} />;
    default:             return <CheckSquare {...props} />;
  }
}

// ── TriggerEventSelect ────────────────────────────────────────────────────────

export function TriggerEventSelect({
  applicationId,
  value,
  onChange,
  showPayload = false,
}: {
  applicationId: string;
  value: string;
  onChange: (key: string) => void;
  showPayload?: boolean;
}) {
  const { data, isError } = useTriggerEvents(applicationId);
  const events = data ?? TRIGGER_FALLBACK;

  const grouped = TRIGGER_CATEGORY_ORDER.reduce<Record<string, TriggerEventCatalogItem[]>>((acc, cat) => {
    const items = events.filter(e => e.category === cat);
    if (items.length) acc[cat] = items;
    return acc;
  }, {});

  const selected = events.find(e => e.key === value);
  const isUnknown = !!value && !selected;

  return (
    <div>
      {isError && (
        <div style={{ fontSize: 12, color: "var(--color-draft)", background: "var(--color-widget-form-bg)", padding: "4px 8px", borderRadius: 4, marginBottom: 6 }}>
          Could not load trigger event catalog — showing defaults only.
        </div>
      )}
      <select value={value} onChange={e => onChange(e.target.value)} style={inputStyle}>
        {isUnknown && <option value={value}>{value} (unknown)</option>}
        {Object.entries(grouped).map(([cat, items]) => (
          <optgroup key={cat} label={TRIGGER_CATEGORY_LABELS[cat as TriggerEventCatalogItem["category"]]}>
            {items.map(ev => (
              <option key={ev.key} value={ev.key} disabled={!ev.enabled}>
                {ev.label}{ev.source_name && ev.source_type !== "system" ? ` — ${ev.source_name}` : ""}
                {!ev.enabled ? " (disabled)" : ""}
              </option>
            ))}
          </optgroup>
        ))}
      </select>
      {isUnknown && (
        <div style={{ fontSize: 12, color: "var(--color-danger)", marginTop: 4 }}>
          Event &quot;{value}&quot; not found in catalog — source may have been deleted.
        </div>
      )}
      {showPayload && selected && selected.payload_schema.length > 0 && (
        <div style={{ marginTop: 8, background: "var(--color-surface-faint)", border: "1px solid var(--color-border)", borderRadius: 6, padding: "8px 10px" }}>
          <div style={{ fontSize: 11, fontWeight: 600, color: "var(--color-text-quiet)", marginBottom: 4, textTransform: "uppercase", letterSpacing: "0.05em" }}>
            Expected context fields
          </div>
          {selected.payload_schema.map(f => (
            <div key={f.key} style={{ display: "flex", gap: 6, alignItems: "baseline", fontSize: 12, color: "var(--color-text-strong)", marginBottom: 2 }}>
              <code style={{ background: "var(--color-border)", padding: "1px 5px", borderRadius: 3, fontSize: 11 }}>{f.key}</code>
              <span style={{ color: "var(--color-text-quiet)" }}>{f.type}</span>
              {f.required && <span style={{ color: "var(--color-danger)", fontSize: 11 }}>required</span>}
            </div>
          ))}
          {selected.description && (
            <div style={{ fontSize: 11, color: "var(--color-disabled)", marginTop: 6 }}>{selected.description}</div>
          )}
        </div>
      )}
    </div>
  );
}

export function StatusBadge({ status }: { status: string }) {
  const c = STATUS_TONES[status] ?? STATUS_TONES.draft;
  return <UiStatusBadge tone={c.tone}>{c.label}</UiStatusBadge>;
}

// ── Shared UI helpers ─────────────────────────────────────────────────────────

export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: React.ReactNode }) {
  return (
    <Dialog open title={title} onClose={onClose} width={420}>
      {children}
    </Dialog>
  );
}

export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <UiField label={label}>{children}</UiField>;
}

export function IconBtn({ title, onClick, disabled, danger, children }: {
  title: string; onClick: () => void; disabled?: boolean; danger?: boolean; children: React.ReactNode
}) {
  return (
    <IconButton
      title={title}
      aria-label={title}
      onClick={onClick}
      disabled={disabled}
      danger={danger}
      size={24}
      style={{ fontSize: 11 }}
    >
      {children}
    </IconButton>
  );
}
