import { type ChartContextDim, dimLabel } from "../../api/client";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { defaultLeafCode } from "../dashboardLayout";

// ── Context selector row ──────────────────────────────────────────────────────

export function ContextSelectors({
  dims,
  context,
  onChange,
  vertical = false,
}: {
  dims: ChartContextDim[];
  context: Record<string, string>;
  onChange: (dimId: string, code: string) => void;
  /** Stack the selectors (for a left/right placement). */
  vertical?: boolean;
}) {
  // dims is already just the context selectors (server excludes the plotted
  // dimension and any hidden members — see chart.go's buildContextDims).
  if (dims.length === 0) return null;
  return (
    <div style={{ display: "flex", flexWrap: vertical ? "nowrap" : "wrap", flexDirection: vertical ? "column" : "row", gap: 8, alignItems: vertical ? "flex-start" : "center", padding: "4px 0" }}>
      {dims.map(dim => (
        <div key={dim.id} style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <label htmlFor={`ctx-${dim.id}`} style={{ fontSize: 12, color: "var(--color-text-muted)", fontWeight: 500, whiteSpace: "nowrap" }}>
            {dimLabel(dim)}:
          </label>
          <HierarchicalMemberSelect
            id={`ctx-${dim.id}`}
            ariaLabel={dimLabel(dim)}
            members={dim.members}
            value={context[dim.id] ?? defaultLeafCode({ members: dim.members }) ?? ""}
            onChange={code => onChange(dim.id, code)}
          />
        </div>
      ))}
    </div>
  );
}
