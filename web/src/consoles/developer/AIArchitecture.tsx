import { useLayoutEffect, useMemo, useRef, useState } from "react";
import type { AITargetModel, AITaskPlan } from "../../api/client";
import { Button, Txt } from "../../ui";

// The plan for approval and its architecture (owner, 2026-11-13): the
// developer approves the whole plan — its stages and the model it builds:
// dimensions → grids (with their dimensions and metrics) → dashboards (with
// the grids they show). Simple and plain: neutral boxes, thin lines, one
// accent for what the pointed-at box is joined to; every link is written in
// the boxes too, so the scheme reads without following lines.

type Dim = { key: string; name: string; members?: string; used: boolean };
type Grid = { key: string; name: string; role: string; dims: string[]; metrics: string[] };
type Page = { key: string; name: string; audience?: string; grids: string[]; guide: boolean };
type Line = { id: string; from: string; to: string; d: string };

const k = (s: string) => s.trim().toLowerCase();

function readArchitecture(t: AITargetModel) {
  const grids = new Map<string, Grid>();
  for (const s of t.sheets ?? []) {
    if (!s.grid || s.role === "guide" || s.role === "skip") continue;
    const g = grids.get(k(s.grid)) ?? { key: k(s.grid), name: s.grid, role: s.role, dims: [], metrics: [] };
    for (const d of s.dimensions ?? []) if (!g.dims.includes(k(d))) g.dims.push(k(d));
    for (const m of s.metrics ?? []) if (!g.metrics.includes(m)) g.metrics.push(m);
    grids.set(g.key, g);
  }
  // Dimensions in the order the grids first use them (fewer crossing
  // lines), then those no grid uses yet.
  const listed = new Map((t.dimensions ?? []).map(d => [k(d.name), d] as const));
  const order: string[] = [];
  for (const g of grids.values()) for (const d of g.dims) if (!order.includes(d)) order.push(d);
  for (const d of listed.keys()) if (!order.includes(d)) order.push(d);
  const dims: Dim[] = order.map(key => {
    const d = listed.get(key);
    const name = d?.name ?? [...grids.values()].flatMap(g => g.dims).find(x => x === key) ?? key;
    return { key, name, members: d?.members, used: [...grids.values()].some(g => g.dims.includes(key)) };
  });
  const pages: Page[] = (t.pages ?? []).map(p =>
    typeof p === "string"
      ? { key: k(p), name: p, grids: [], guide: false }
      : { key: k(p.name), name: p.name, audience: p.audience, grids: [...new Set((p.grids ?? []).map(k))], guide: !!p.guide });
  return { dims, grids: [...grids.values()], pages };
}

/** The architecture drawn: dimensions → grids → dashboards. */
export function ArchitectureScheme({ target }: { target: AITargetModel }) {
  const arch = useMemo(() => readArchitecture(target), [target]);
  const [hover, setHover] = useState<string | null>(null);
  const [allMetrics, setAllMetrics] = useState<Record<string, boolean>>({});
  const box = useRef<HTMLDivElement>(null);
  const els = useRef(new Map<string, HTMLElement>());
  const [lines, setLines] = useState<Line[]>([]);
  const [size, setSize] = useState({ w: 0, h: 0 });
  const [narrow, setNarrow] = useState(false);

  useLayoutEffect(() => {
    const root = box.current;
    if (!root) return;
    const measure = () => {
      const r = root.getBoundingClientRect();
      setNarrow(r.width < 720);
      const at = (id: string, side: "l" | "r") => {
        const e = els.current.get(id);
        if (!e) return null;
        const b = e.getBoundingClientRect();
        return { x: (side === "r" ? b.right : b.left) - r.left, y: b.top + Math.min(b.height / 2, 18) - r.top };
      };
      const out: Line[] = [];
      const join = (from: string, to: string) => {
        const a = at(from, "r"), b = at(to, "l");
        if (!a || !b) return;
        const mid = (a.x + b.x) / 2;
        out.push({ id: `${from}>${to}`, from, to, d: `M${a.x},${a.y} C${mid},${a.y} ${mid},${b.y} ${b.x},${b.y}` });
      };
      for (const g of arch.grids) for (const d of g.dims) join(`dim:${d}`, `grid:${g.key}`);
      for (const p of arch.pages) for (const g of p.grids) join(`grid:${g}`, `page:${p.key}`);
      setLines(out);
      setSize({ w: root.scrollWidth, h: root.scrollHeight });
    };
    const frame = requestAnimationFrame(measure);
    const ro = new ResizeObserver(() => requestAnimationFrame(measure));
    ro.observe(root);
    return () => { cancelAnimationFrame(frame); ro.disconnect(); };
  }, [arch, allMetrics]);

  // What the pointed-at box is joined to.
  const related = useMemo(() => {
    if (!hover) return null;
    const set = new Set<string>([hover]);
    const [kind, key] = [hover.slice(0, hover.indexOf(":")), hover.slice(hover.indexOf(":") + 1)];
    const gridsOf = (g: string) => { set.add(`grid:${g}`); for (const d of arch.grids.find(x => x.key === g)?.dims ?? []) set.add(`dim:${d}`); };
    if (kind === "dim") {
      for (const g of arch.grids) if (g.dims.includes(key)) {
        set.add(`grid:${g.key}`);
        for (const p of arch.pages) if (p.grids.includes(g.key)) set.add(`page:${p.key}`);
      }
    } else if (kind === "grid") {
      gridsOf(key);
      for (const p of arch.pages) if (p.grids.includes(key)) set.add(`page:${p.key}`);
    } else {
      for (const g of arch.pages.find(x => x.key === key)?.grids ?? []) gridsOf(g);
    }
    return set;
  }, [hover, arch]);

  if (arch.grids.length === 0 && arch.pages.length === 0) {
    return <div className="mvx-admin-muted"><Txt>{"No architecture recorded yet."}</Txt></div>;
  }
  const faded = (id: string) => (related && !related.has(id) ? 0.35 : 1);
  const reg = (id: string) => (e: HTMLElement | null) => { if (e) els.current.set(id, e); else els.current.delete(id); };
  const point = (id: string) => ({ onMouseEnter: () => setHover(id), onMouseLeave: () => setHover(null) });
  const boxStyle = (id: string) => ({
    border: `1px solid ${related?.has(id) ? "var(--color-brand-500)" : "var(--color-border)"}`, borderRadius: 6, background: "var(--color-surface)",
    padding: "6px 9px", fontSize: 12, opacity: faded(id),
  });
  const head = { fontSize: 11, fontWeight: 600, color: "var(--color-text-muted)", margin: "0 0 6px" };
  const gridName = (key: string) => arch.grids.find(g => g.key === key)?.name ?? key;
  const dimName = (key: string) => arch.dims.find(d => d.key === key)?.name ?? key;

  return (
    <div ref={box} style={{ position: "relative", display: "grid", gap: narrow ? 16 : "0 56px",
      gridTemplateColumns: narrow ? "1fr" : "minmax(140px, 1fr) minmax(240px, 1.8fr) minmax(160px, 1fr)" }}>
      {!narrow && (
        <svg aria-hidden width={size.w} height={size.h} style={{ position: "absolute", inset: 0, pointerEvents: "none" }}>
          {lines.map(l => {
            const on = !!related && related.has(l.from) && related.has(l.to);
            return <path key={l.id} d={l.d} fill="none" strokeWidth={on ? 1.6 : 1}
              stroke={on ? "var(--color-brand-500)" : "var(--color-border)"} opacity={related && !on ? 0.3 : 1} />;
          })}
        </svg>
      )}
      <div>
        <p style={head}><Txt>{`Dimensions (${arch.dims.length})`}</Txt></p>
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          {arch.dims.map(d => (
            <div key={d.key} ref={reg(`dim:${d.key}`)} {...point(`dim:${d.key}`)} style={boxStyle(`dim:${d.key}`)}>
              <strong><Txt>{d.name}</Txt></strong>
              {d.members && <div className="mvx-admin-muted" style={{ fontSize: 11 }}><Txt>{d.members}</Txt></div>}
              {!d.used && <div className="mvx-admin-muted" style={{ fontSize: 11 }}><Txt>{"on no grid yet"}</Txt></div>}
            </div>
          ))}
        </div>
      </div>
      <div>
        <p style={head}><Txt>{`Grids and metrics (${arch.grids.length})`}</Txt></p>
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          {arch.grids.map(g => {
            const all = !!allMetrics[g.key];
            return (
              <div key={g.key} ref={reg(`grid:${g.key}`)} {...point(`grid:${g.key}`)} style={boxStyle(`grid:${g.key}`)}>
                <div style={{ display: "flex", gap: 8 }}>
                  <strong style={{ flex: 1 }}><Txt>{g.name}</Txt></strong>
                  <span className="mvx-admin-muted" style={{ fontSize: 11 }}><Txt>{g.role}</Txt></span>
                </div>
                <div className="mvx-admin-muted" style={{ fontSize: 11 }}>
                  <Txt>{g.dims.length > 0 ? `by ${g.dims.map(dimName).join(" × ")}` : "single values, no dimensions"}</Txt>
                </div>
                {g.metrics.length > 0 && (
                  <div style={{ fontSize: 11, marginTop: 2 }}>
                    <Txt>{(all ? g.metrics : g.metrics.slice(0, 6)).join(", ")}</Txt>
                    {g.metrics.length > 6 && (
                      <button type="button" className="mvx-link-button" style={{ marginLeft: 6, fontSize: 11 }}
                        onClick={() => setAllMetrics(s => ({ ...s, [g.key]: !all }))}>
                        <Txt>{all ? "fewer" : `+${g.metrics.length - 6} more`}</Txt>
                      </button>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </div>
      <div>
        <p style={head}><Txt>{`Dashboards (${arch.pages.length})`}</Txt></p>
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          {arch.pages.map(p => (
            <div key={p.key} ref={reg(`page:${p.key}`)} {...point(`page:${p.key}`)} style={boxStyle(`page:${p.key}`)}>
              <strong><Txt>{p.name}</Txt></strong>
              {p.audience && <span className="mvx-admin-muted" style={{ fontSize: 11 }}><Txt>{` · ${p.audience}`}</Txt></span>}
              <div className="mvx-admin-muted" style={{ fontSize: 11 }}>
                <Txt>{p.grids.length > 0 ? `shows ${p.grids.map(gridName).join(", ")}` : p.guide ? "guide, text only" : "grids not named yet"}</Txt>
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

/** The plan for approval, in the conversation: the goal, every stage, the
 *  architecture, and Approve plan / Change. */
export function PlanReviewCard({ plan, approving, onApprove, onChange }: {
  plan: AITaskPlan; approving: boolean; onApprove?: () => void; onChange: () => void;
}) {
  const t = plan.target;
  const open = (t?.decisions ?? []).filter(d => !d.choice);
  const section = { fontSize: 12, fontWeight: 600, margin: "14px 0 6px" };
  return (
    <div data-plan-review className="mvx-admin-object" style={{ margin: "12px 0", fontSize: 13 }}>
      <div className="mvx-admin-object__header"><strong><Txt>{"Plan for your approval"}</Txt></strong></div>
      <div style={{ padding: "12px 16px" }}>
        <div><Txt>{plan.goal}</Txt></div>
        {t?.summary && <div className="mvx-admin-muted" style={{ marginTop: 4 }}><Txt>{t.summary}</Txt></div>}
        <p style={section}><Txt>{`Stages (${plan.stages.length})`}</Txt></p>
        <ol style={{ margin: 0, padding: 0, listStyle: "none" }}>
          {plan.stages.map((s, i) => (
            <li key={i} style={{ marginBottom: 4 }}>
              <Txt>{`${i + 1}. ${s.title}`}</Txt>
              {(s.items?.length ?? 0) > 0 && <div className="mvx-admin-muted" style={{ fontSize: 12, paddingLeft: 16 }}><Txt>{s.items.join("; ")}</Txt></div>}
            </li>
          ))}
        </ol>
        <p style={section}><Txt>{"Architecture"}</Txt></p>
        {t ? <ArchitectureScheme target={t} /> : <div className="mvx-admin-muted"><Txt>{"No architecture recorded yet."}</Txt></div>}
        {open.length > 0 && (
          <p style={{ margin: "12px 0 0" }}><Txt>{`Waiting for your decision: ${open.map(d => d.question).join("; ")}`}</Txt></p>
        )}
      </div>
      <div style={{ display: "flex", gap: 8, padding: "10px 16px", borderTop: "1px solid var(--color-border)" }}>
        <Button size="sm" variant="primary" loading={approving} loadingLabel="Approving…" disabled={!onApprove} onClick={onApprove}>Approve plan</Button>
        <Button size="sm" onClick={onChange}>Change…</Button>
        <span className="mvx-admin-muted" style={{ alignSelf: "center", fontSize: 12 }}><Txt>{"Nothing is built until you approve."}</Txt></span>
      </div>
    </div>
  );
}
