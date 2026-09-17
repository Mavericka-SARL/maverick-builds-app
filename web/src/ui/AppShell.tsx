import React, { useState } from "react";
import { PanelLeftClose, PanelLeftOpen } from "lucide-react";
import { ContextBar } from "./ContextBar";
import { SidebarNav } from "./SidebarNav";
import type { ContextBarItem, NavGroup, NavItem } from "./types";

interface AppShellProps {
  /**
   * Optional product name above the subtitle. Omitted by default: the
   * subtitle already names where you are ("Developer console"), and stacking
   * a product name on top of it made the brand block two lines tall, which no
   * longer lined up with the context bar across the top of the main area.
   * Pass one only where the product name genuinely adds information.
   */
  productName?: string;
  productSubtitle?: string;
  /** A tenant's logo (white-labelling), shown before the brand text. */
  logo?: React.ReactNode;
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

export function AppShell({
  productName,
  logo,
  productSubtitle = "Planning workspace",
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
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      <aside className="mvx-app-shell__sidebar">
        <div className="mvx-app-shell__brand">
          {logo && <div className="mvx-app-shell__brand-logo">{logo}</div>}
          <div className="mvx-app-shell__brand-text">
            {productName && <h1 className="mvx-app-shell__brand-name">{productName}</h1>}
            {productSubtitle && (
              productName
                ? <div className="mvx-app-shell__brand-subtitle">{productSubtitle}</div>
                // With no product name above it, the subtitle IS the heading —
                // so it becomes the h1 rather than leaving the sidebar with no
                // heading element at all.
                : <h1 className="mvx-app-shell__brand-name">{productSubtitle}</h1>
            )}
          </div>
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
