import { StatusBadge } from "./StatusBadge";
import type React from "react";
import type { ContextBarItem } from "./types";

interface ContextBarProps {
  items: ContextBarItem[];
  actions?: React.ReactNode;
  className?: string;
}

export function ContextBar({ items, actions, className }: ContextBarProps) {
  if (items.length === 0 && !actions) return null;

  return (
    <div className={["mvx-context-bar", className].filter(Boolean).join(" ")}>
      {items.map((item) => (
        <div key={item.id} className="mvx-context-bar__item">
          {item.label && <span className="mvx-context-bar__label">{item.label}</span>}
          {item.tone ? (
            <StatusBadge tone={item.tone}>{item.value}</StatusBadge>
          ) : (
            <span className="mvx-context-bar__value">{item.value}</span>
          )}
        </div>
      ))}
      {actions && <div className="mvx-context-bar__actions">{actions}</div>}
    </div>
  );
}

export type { ContextBarItem };
