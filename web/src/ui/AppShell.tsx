import React, { useState } from "react";
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
