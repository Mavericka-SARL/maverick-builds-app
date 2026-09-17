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

export function SidebarNav({ groups, activeId, onSelect, className, collapsed }: SidebarNavProps) {
  return (
    <nav className={["mvx-sidebar-nav", className].filter(Boolean).join(" ")} aria-label="Primary">
      {groups.map((group, index) => (
        <div key={group.id ?? group.label ?? index} className="mvx-sidebar-nav__group">
          {group.label && <div className="mvx-sidebar-nav__group-label">{group.label}</div>}
          <div className="mvx-sidebar-nav__items">
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
        </div>
      ))}
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
