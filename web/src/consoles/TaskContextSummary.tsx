import type { TaskContextEntry } from "../api/client";

// The context block of an approval/task/history card. Prefers the server's
// context_display (member codes resolved to labels, metric ids to names —
// "country · geography: Canada (CA)") and falls back to the raw context map
// so older payloads and unschema'd keys still render.
export function TaskContextSummary({
  contextDisplay,
  context,
}: {
  contextDisplay?: TaskContextEntry[];
  context?: Record<string, unknown>;
}) {
  const entries: TaskContextEntry[] = contextDisplay?.length
    ? contextDisplay
    : Object.entries(context ?? {})
        .filter(([k]) => !k.startsWith("_"))
        .map(([k, v]) => ({ key: k, value: String(v), display: String(v) }));
  if (entries.length === 0) return null;
  return (
    <div style={{ background: "var(--color-surface-subtle)", borderRadius: "var(--radius-button)", padding: "8px 12px", fontSize: 12 }}>
      {entries.slice(0, 5).map((e) => (
        <div key={e.key} style={{ display: "flex", gap: 8, marginBottom: 2 }}>
          <span className="mvx-admin-muted" style={{ minWidth: 100 }}>
            {e.key}
            {e.dimension_name && e.dimension_name.toLowerCase() !== e.key.toLowerCase() && (
              <span> · {e.dimension_name}</span>
            )}
          </span>
          <span style={{ fontWeight: 500 }}>
            {e.display}
            {/* The code stays visible when a name resolved — it is the stable
                identifier people paste into filters and imports. */}
            {e.display !== e.value && <span className="mvx-admin-muted"> ({e.value})</span>}
          </span>
        </div>
      ))}
    </div>
  );
}
