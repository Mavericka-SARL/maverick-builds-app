import type { WorkflowStepDef } from "../../api/client";
import { StepTypeIcon } from "./WorkflowShared";
import { STEP_TYPE_LABELS } from "./workflowConstants";

// ── ProcessCanvas ─────────────────────────────────────────────────────────────

/** Returns non-end step-def IDs that `step` routes to. */
function getRouteTargets(step: WorkflowStepDef): string[] {
  const routes = step.routes ?? {};
  const targets = Object.values(routes).filter((v): v is string => !!v && !v.startsWith("end-"));
  return [...new Set(targets)];
}

/**
 * Topologically-ordered rows for the canvas.
 * Each row is an array of steps; rows with >1 step are rendered as parallel lanes.
 *
 * Multiple steps share a row only when they are explicitly all targets of the same
 * step's routes (parallel branches). Unconnected steps fall back to array order,
 * so newly-added steps without routes appear sequentially, not side-by-side.
 */
function computeCanvasRows(steps: WorkflowStepDef[]): WorkflowStepDef[][] {
  if (steps.length === 0) return [];

  const stepMap = new Map(steps.map(s => [s.id, s]));
  const rows: WorkflowStepDef[][] = [];
  const visited = new Set<string>();

  // Next unvisited step in array order — used to continue after a dead-end.
  const seedNext = (): string | null => steps.find(s => !visited.has(s.id))?.id ?? null;

  let frontier: string[] = [];
  const first = seedNext();
  if (first) frontier = [first];

  while (true) {
    frontier = frontier.filter(id => !visited.has(id) && stepMap.has(id));

    if (frontier.length === 0) {
      const next = seedNext();
      if (!next) break;
      frontier = [next];
    }

    frontier.forEach(id => visited.add(id));
    rows.push(frontier.map(id => stepMap.get(id)!));

    const next = new Set<string>();
    for (const id of frontier) {
      for (const t of getRouteTargets(stepMap.get(id)!)) {
        if (!visited.has(t) && stepMap.has(t)) next.add(t);
      }
    }
    frontier = [...next];
  }

  return rows;
}

export function ProcessCanvas({ steps, selectedId, onSelect }: {
  steps: WorkflowStepDef[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  const rows = computeCanvasRows(steps);

  return (
    <div style={{ padding: "32px 0", display: "flex", flexDirection: "column", alignItems: "center", minHeight: "100%" }}>
      <ProcessNode label="Start" icon="▶" color="var(--color-text-quiet)" />
      {steps.length === 0 && (
        <div style={{ color: "var(--color-disabled)", fontSize: 13, padding: "20px 0" }}>Add steps to build the workflow.</div>
      )}

      {rows.map((row, rowIdx) => {
        const isParallel = row.length > 1;
        return (
          <div key={rowIdx} style={{ display: "flex", flexDirection: "column", alignItems: "center", width: "100%" }}>
            {isParallel ? (
              <>
                {/* Fork bar */}
                <div style={{ width: 2, height: 20, background: "var(--color-border-muted)" }} />
                <div style={{ display: "flex", alignItems: "flex-start", gap: 16, position: "relative" }}>
                  {/* Horizontal bar spanning all branches */}
                  <div style={{
                    position: "absolute", top: 0, left: "50%",
                    transform: "translateX(-50%)",
                    width: `calc(100% - 56px)`, height: 2, background: "var(--color-border-muted)",
                  }} />
                  {row.map((step) => (
                    <div key={step.id} style={{ display: "flex", flexDirection: "column", alignItems: "center" }}>
                      <div style={{ width: 2, height: 20, background: "var(--color-border-muted)" }} />
                      <ProcessStepCard step={step} selected={selectedId === step.id} onClick={() => onSelect(step.id)} />
                      {step.type === "approval" && (
                        <div style={{ display: "flex", gap: 16, marginTop: 6 }}>
                          <div style={{ fontSize: 10, color: "var(--color-success)" }}>✓ {step.routes?.approve ? routeLabel(step.routes.approve) : "?"}</div>
                          <div style={{ fontSize: 10, color: "var(--color-danger)" }}>✕ {step.routes?.reject ? routeLabel(step.routes.reject) : "?"}</div>
                        </div>
                      )}
                      <div style={{ width: 2, height: 20, background: "var(--color-border-muted)" }} />
                    </div>
                  ))}
                </div>
                {/* Join bar */}
                <div style={{
                  width: `calc(${Math.min(row.length * 296, 900)}px - 56px)`, height: 2, background: "var(--color-border-muted)",
                  alignSelf: "center",
                }} />
              </>
            ) : (
              <>
                <Connector />
                <ProcessStepCard
                  step={row[0]}
                  selected={selectedId === row[0].id}
                  onClick={() => onSelect(row[0].id)}
                />
                {row[0].type === "condition" && (
                  <div style={{ display: "flex", gap: 40, marginTop: 8 }}>
                    <div style={{ fontSize: 11, color: "var(--color-success)" }}>True → {row[0].routes?.true ? routeLabel(row[0].routes.true) : "?"}</div>
                    <div style={{ fontSize: 11, color: "var(--color-danger)" }}>False → {row[0].routes?.false ? routeLabel(row[0].routes.false) : "?"}</div>
                  </div>
                )}
                {row[0].type === "approval" && (
                  <div style={{ display: "flex", gap: 40, marginTop: 8 }}>
                    <div style={{ fontSize: 11, color: "var(--color-success)" }}>Approve → {row[0].routes?.approve ? routeLabel(row[0].routes.approve) : "?"}</div>
                    <div style={{ fontSize: 11, color: "var(--color-danger)" }}>Reject → {row[0].routes?.reject ? routeLabel(row[0].routes.reject) : "?"}</div>
                  </div>
                )}
              </>
            )}
          </div>
        );
      })}

      <Connector />
      <ProcessNode label="End" icon="⚑" color="var(--color-disabled)" />
    </div>
  );
}

function routeLabel(target: string): string {
  if (target.startsWith("end-")) return endStateLabel(target);
  return target;
}

function endStateLabel(s: string): string {
  return s.replace("end-", "").replace(/-/g, " ").replace(/^\w/, c => c.toUpperCase());
}

function ProcessNode({ label, icon, color }: { label: string; icon: string; color: string }) {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8, padding: "6px 16px", borderRadius: 20, border: `2px solid ${color}`, color, fontSize: 13, fontWeight: 600 }}>
      <span>{icon}</span> {label}
    </div>
  );
}

function ProcessStepCard({ step, selected, onClick }: { step: WorkflowStepDef; selected: boolean; onClick: () => void }) {
  return (
    <div
      onClick={onClick}
      style={{
        width: 280, padding: "12px 16px", borderRadius: 10,
        border: selected ? "2px solid var(--color-selected)" : "2px solid var(--color-border)",
        background: selected ? "var(--color-selected-bg)" : "var(--color-surface)",
        cursor: "pointer", boxShadow: "0 1px 4px rgba(0,0,0,0.06)",
      }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 4 }}>
        <StepTypeIcon type={step.type} size={16} />
        <div>
          <div style={{ fontSize: 14, fontWeight: 600, color: "var(--color-text)" }}>{step.name}</div>
          <div style={{ fontSize: 11, color: "var(--color-disabled)" }}>{STEP_TYPE_LABELS[step.type]}</div>
        </div>
      </div>
      {step.assignee_roles && step.assignee_roles.length > 0 && (
        <div style={{ fontSize: 12, color: "var(--color-text-quiet)", marginTop: 4 }}>
          Assigned to: {step.assignee_roles.join(", ")}
        </div>
      )}
      {step.sla_hours && (
        <div style={{ fontSize: 12, color: "var(--color-text-quiet)" }}>SLA: {step.sla_hours}h</div>
      )}
    </div>
  );
}

function Connector() {
  return <div style={{ width: 2, height: 28, background: "var(--color-border-muted)" }} />;
}
