import React, { useRef, useState } from "react";
import { PanelLeftClose, PanelLeftOpen } from "lucide-react";
import { ContextBar } from "./ContextBar";
import { SidebarNav } from "./SidebarNav";
import type { ContextBarItem, NavGroup, NavItem } from "./types";

interface AppShellProps {
  /**
   * The brand block at the head of the sidebar — the product mark, or a
   * tenant's own logo and name where white-labelling is configured (see
   * branding/BrandMark). It is one line high and the same for every role:
   * the sidebar groups below it already say which parts of the product the
   * person holds, so the head never names a console.
   */
  brand?: React.ReactNode;
  navGroups: NavGroup[];
  activeNavId?: string;
  onNavSelect?: (item: NavItem) => void;
  contextItems?: ContextBarItem[];
  contextActions?: React.ReactNode;
  utility?: React.ReactNode;
  sidebarFooter?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}

// One shared key: the collapse choice is about the person's screen, not
// about which console they happen to be in, so switching personas keeps it.
const SIDEBAR_COLLAPSED_KEY = "mvx.sidebar.collapsed";
// The dragged width, for the same reason. Absent means the CSS default
// (--sidebar-width), which double-clicking the handle goes back to.
const SIDEBAR_WIDTH_KEY = "mvx.sidebar.width";
const SIDEBAR_DEFAULT_WIDTH = 260;
const SIDEBAR_MIN_WIDTH = 200;
const SIDEBAR_MAX_WIDTH = 480;
const KEYBOARD_STEP = 16;

const clampWidth = (w: number) => Math.round(Math.min(SIDEBAR_MAX_WIDTH, Math.max(SIDEBAR_MIN_WIDTH, w)));

export function AppShell({
  brand,
  navGroups,
  activeNavId,
  onNavSelect,
  contextItems = [],
  contextActions,
  utility,
  sidebarFooter,
  children,
  className,
}: AppShellProps) {
  const [collapsed, setCollapsed] = useState(() => {
    try {
      return localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "1";
    } catch {
      return false;
    }
  });

  const [width, setWidth] = useState<number | null>(() => {
    try {
      const stored = Number(localStorage.getItem(SIDEBAR_WIDTH_KEY));
      return stored > 0 ? clampWidth(stored) : null;
    } catch {
      return null;
    }
  });
  const [resizing, setResizing] = useState(false);
  const sidebarRef = useRef<HTMLElement>(null);

  const saveWidth = (next: number | null) => {
    setWidth(next);
    try {
      if (next === null) localStorage.removeItem(SIDEBAR_WIDTH_KEY);
      else localStorage.setItem(SIDEBAR_WIDTH_KEY, String(next));
    } catch {
      // As with the collapse toggle: the width still applies, it just isn't remembered.
    }
  };

  // While dragging, the width goes straight onto the sidebar's style so the
  // console behind it does not re-render on every pointer move; it becomes
  // state (and is remembered) when the pointer is released.
  const startResize = (event: React.PointerEvent<HTMLDivElement>) => {
    const sidebar = sidebarRef.current;
    if (event.button !== 0 || !sidebar) return;
    event.preventDefault();
    const handle = event.currentTarget;
    handle.setPointerCapture(event.pointerId);
    const startX = event.clientX;
    const startWidth = sidebar.getBoundingClientRect().width;
    let current = startWidth;
    setResizing(true);
    const move = (e: PointerEvent) => {
      current = clampWidth(startWidth + e.clientX - startX);
      sidebar.style.setProperty("--sidebar-width", `${current}px`);
    };
    const end = () => {
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", end);
      handle.removeEventListener("pointercancel", end);
      setResizing(false);
      if (current !== startWidth) saveWidth(current);
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", end);
    handle.addEventListener("pointercancel", end);
  };

  const resizeByKey = (event: React.KeyboardEvent<HTMLDivElement>) => {
    const now = width ?? SIDEBAR_DEFAULT_WIDTH;
    const next =
      event.key === "ArrowLeft" ? now - KEYBOARD_STEP
      : event.key === "ArrowRight" ? now + KEYBOARD_STEP
      : event.key === "Home" ? SIDEBAR_MIN_WIDTH
      : event.key === "End" ? SIDEBAR_MAX_WIDTH
      : null;
    if (next === null) return;
    event.preventDefault();
    saveWidth(clampWidth(next));
  };

  const toggleCollapsed = () => {
    setCollapsed((prev) => {
      const next = !prev;
      try {
        localStorage.setItem(SIDEBAR_COLLAPSED_KEY, next ? "1" : "0");
      } catch {
        // Private-mode storage failures just lose persistence, not the toggle.
      }
      return next;
    });
  };

  return (
    <div
      className={[
        "mvx-app-shell",
        collapsed ? "mvx-app-shell--sidebar-collapsed" : "",
        resizing ? "mvx-app-shell--resizing" : "",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      <aside
        ref={sidebarRef}
        className="mvx-app-shell__sidebar"
        style={width === null ? undefined : ({ "--sidebar-width": `${width}px` } as React.CSSProperties)}
      >
        <div className="mvx-app-shell__brand">
          {brand && (
            <div className="mvx-app-shell__brand-text">
              <h1 className="mvx-app-shell__brand-name">{brand}</h1>
            </div>
          )}
          <button
            type="button"
            className="mvx-app-shell__collapse"
            onClick={toggleCollapsed}
            aria-expanded={!collapsed}
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          >
            {collapsed ? <PanelLeftOpen size={16} /> : <PanelLeftClose size={16} />}
          </button>
        </div>
        <div className="mvx-app-shell__nav">
          <SidebarNav groups={navGroups} activeId={activeNavId} onSelect={onNavSelect} collapsed={collapsed} />
        </div>
        {sidebarFooter && <div className="mvx-app-shell__footer">{sidebarFooter}</div>}
        {!collapsed && (
          <div
            className="mvx-app-shell__resize"
            role="separator"
            aria-orientation="vertical"
            aria-label="Resize sidebar"
            aria-valuemin={SIDEBAR_MIN_WIDTH}
            aria-valuemax={SIDEBAR_MAX_WIDTH}
            aria-valuenow={width ?? SIDEBAR_DEFAULT_WIDTH}
            tabIndex={0}
            title="Drag to resize · double-click to reset"
            onPointerDown={startResize}
            onKeyDown={resizeByKey}
            onDoubleClick={() => saveWidth(null)}
          />
        )}
      </aside>
      <div className="mvx-app-shell__main">
        <ContextBar
          items={contextItems}
          actions={
            contextActions || utility ? (
              <>
                {contextActions}
                {utility}
              </>
            ) : undefined
          }
        />
        {children}
      </div>
    </div>
  );
}

export type { AppShellProps };
