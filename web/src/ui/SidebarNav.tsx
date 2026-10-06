import { useState } from "react";
import { ChevronDown } from "lucide-react";
import type { NavGroup, NavItem } from "./types";

interface SidebarNavProps {
  groups: NavGroup[];
  activeId?: string;
  onSelect?: (item: NavItem) => void;
  className?: string;
  /** Icon-only rail mode: labels/group headers are hidden by CSS, so each
      item carries its label as a tooltip instead. */
  collapsed?: boolean;
}

// Which groups are folded, by id or label. Like the sidebar's own collapse
// it is about the person's screen, not one console, so it is one key.
const FOLDED_GROUPS_KEY = "mvx.sidebar.foldedGroups";

function readFolded(): string[] {
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(FOLDED_GROUPS_KEY) ?? "[]");
    return Array.isArray(parsed) ? parsed.filter((k): k is string => typeof k === "string") : [];
  } catch {
    return [];
  }
}

export function SidebarNav({ groups, activeId, onSelect, className, collapsed }: SidebarNavProps) {
  const [folded, setFolded] = useState<string[]>(readFolded);

  const toggleFolded = (key: string) => {
    setFolded((prev) => {
      const next = prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key];
      try {
        localStorage.setItem(FOLDED_GROUPS_KEY, JSON.stringify(next));
      } catch {
        // Private-mode storage failures just lose persistence, not the fold.
      }
      return next;
    });
  };

  return (
    <nav className={["mvx-sidebar-nav", className].filter(Boolean).join(" ")} aria-label="Primary">
      {groups.map((group, index) => {
        const key = group.id ?? group.label;
        // The rail has no group headers to unfold with, so it shows every item.
        const isFolded = !collapsed && !!key && folded.includes(key);
        const itemsId = `mvx-sidebar-group-${index}`;
        // A folded group still says it holds the screen that is open.
        const holdsActive = isFolded && group.items.some((item) => item.id === activeId);
        return (
          <div key={key ?? index} className="mvx-sidebar-nav__group">
            {group.label && (collapsed ? (
              <div className="mvx-sidebar-nav__group-label">{group.label}</div>
            ) : (
              <button
                type="button"
                className={["mvx-sidebar-nav__group-label", "mvx-sidebar-nav__group-toggle", holdsActive ? "mvx-sidebar-nav__group-label--active" : ""].filter(Boolean).join(" ")}
                aria-expanded={!isFolded}
                aria-controls={itemsId}
                title={isFolded ? `Show ${group.label}` : `Hide ${group.label}`}
                onClick={() => key && toggleFolded(key)}
              >
                {group.label}
                <ChevronDown size={12} aria-hidden="true" className="mvx-sidebar-nav__group-chevron" />
              </button>
            ))}
            {!isFolded && (
              <div id={itemsId} className="mvx-sidebar-nav__items">
                {group.items.map((item) => (
                  <SidebarNavItem
                    key={item.id}
                    item={item}
                    active={item.id === activeId}
                    onSelect={onSelect}
                    collapsed={collapsed}
                  />
                ))}
              </div>
            )}
          </div>
        );
      })}
    </nav>
  );
}

function SidebarNavItem({
  item,
  active,
  onSelect,
  collapsed,
}: {
  item: NavItem;
  active: boolean;
  onSelect?: (item: NavItem) => void;
  collapsed?: boolean;
}) {
  const content = (
    <>
      {item.icon && <span className="mvx-sidebar-nav__icon">{item.icon}</span>}
      <span className="mvx-sidebar-nav__label">{item.label}</span>
      {item.badge}
    </>
  );

  const className = ["mvx-sidebar-nav__item", active ? "mvx-sidebar-nav__item--active" : ""]
    .filter(Boolean)
    .join(" ");

  if (item.href) {
    return (
      <a
        className={className}
        href={item.href}
        title={collapsed ? item.label : undefined}
        aria-current={active ? "page" : undefined}
        aria-disabled={item.disabled || undefined}
        onClick={(event) => {
          if (item.disabled) {
            event.preventDefault();
            return;
          }
          onSelect?.(item);
        }}
      >
        {content}
      </a>
    );
  }

  return (
    <button
      type="button"
      className={className}
      disabled={item.disabled}
      title={collapsed ? item.label : undefined}
      aria-current={active ? "page" : undefined}
      onClick={() => onSelect?.(item)}
    >
      {content}
    </button>
  );
}

export type { NavGroup, NavItem };
