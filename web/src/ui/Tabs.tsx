import React, { useCallback, useEffect, useRef, useState } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";

interface Tab<T extends string> {
  id: T;
  label: string;
  count?: number;
}

interface TabsProps<T extends string> {
  tabs: Tab<T>[];
  active: T;
  onChange: (id: T) => void;
  style?: React.CSSProperties;
  /** One line that scrolls sideways, with an arrow at an edge that hides tabs and the active tab kept in view
      (owner, 2026-11-27: thirty dashboards wrapped to six lines of tabs). Without it, tabs wrap as before. */
  scroll?: boolean;
}

export function Tabs<T extends string>({ tabs, active, onChange, style, scroll }: TabsProps<T>) {
  const row = useRef<HTMLDivElement>(null);
  const [edges, setEdges] = useState({ left: false, right: false });
  const measure = useCallback(() => {
    const el = row.current;
    if (!el) return;
    const left = el.scrollLeft > 1;
    const right = el.scrollLeft + el.clientWidth < el.scrollWidth - 1;
    setEdges(prev => (prev.left === left && prev.right === right ? prev : { left, right }));
  }, []);
  useEffect(() => {
    const el = row.current;
    if (!scroll || !el) return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [scroll, measure, tabs.length]);
  // The active tab in view: opened from a link or a search, it may sit past an edge.
  useEffect(() => {
    const el = row.current;
    if (!scroll || !el) return;
    const btn = el.querySelector<HTMLElement>('[aria-pressed="true"]');
    if (!btn) return;
    if (btn.offsetLeft < el.scrollLeft) el.scrollLeft = Math.max(0, btn.offsetLeft - 32);
    else if (btn.offsetLeft + btn.offsetWidth > el.scrollLeft + el.clientWidth) el.scrollLeft = btn.offsetLeft + btn.offsetWidth - el.clientWidth + 32;
    measure();
  }, [scroll, active, measure]);
  const nudge = (dir: 1 | -1) => {
    const el = row.current;
    if (el) el.scrollBy({ left: dir * Math.max(200, el.clientWidth * 0.6), behavior: "smooth" });
  };

  const buttons = tabs.map((tab) => {
    const isActive = tab.id === active;
    return (
      <button
        key={tab.id}
        onClick={() => onChange(tab.id)}
        className={["mvx-tabs__button", isActive ? "mvx-tabs__button--active" : ""].filter(Boolean).join(" ")}
        aria-pressed={isActive}
      >
        {tab.label}
        {tab.count !== undefined && (
          <span className={["mvx-tabs__count", isActive ? "mvx-tabs__count--active" : ""].filter(Boolean).join(" ")}>
            {tab.count}
          </span>
        )}
      </button>
    );
  });
  if (!scroll) return <div className="mvx-tabs" style={style}>{buttons}</div>;
  return (
    <div className="mvx-tabs-scroll" style={style}>
      {edges.left && (
        <button type="button" className="mvx-tabs-scroll__arrow mvx-tabs-scroll__arrow--left" aria-label="Show earlier tabs" title="Earlier tabs" onClick={() => nudge(-1)}>
          <ChevronLeft size={16} />
        </button>
      )}
      <div ref={row} className="mvx-tabs mvx-tabs--scroll" onScroll={measure}>{buttons}</div>
      {edges.right && (
        <button type="button" className="mvx-tabs-scroll__arrow mvx-tabs-scroll__arrow--right" aria-label="Show more tabs" title="More tabs" onClick={() => nudge(1)}>
          <ChevronRight size={16} />
        </button>
      )}
    </div>
  );
}
