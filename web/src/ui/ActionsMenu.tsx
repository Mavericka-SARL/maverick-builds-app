import React, { useEffect, useState } from "react";
import { MoreVertical } from "lucide-react";
import { IconButton } from "./IconButton";

export interface ActionsMenuItem {
  label: string;
  icon?: React.ReactNode;
  onSelect: () => void;
  disabled?: boolean;
}

/**
 * A "⋯" button opening a short menu of a panel's secondary actions (export,
 * import, sync), so its header keeps one primary action in view. The menu is
 * positioned from the button, fixed, so a scrolling or clipping panel does
 * not cut it off; a click elsewhere or Escape closes it.
 */
export function ActionsMenu({ label, items }: { label: string; items: ActionsMenuItem[] }) {
  const [pos, setPos] = useState<{ top: number; right: number } | null>(null);
  useEffect(() => {
    if (!pos) return;
    const close = () => setPos(null);
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") close(); };
    // Deferred so the opening click does not close it at once.
    const t = setTimeout(() => document.addEventListener("click", close), 0);
    document.addEventListener("keydown", onKey);
    return () => { clearTimeout(t); document.removeEventListener("click", close); document.removeEventListener("keydown", onKey); };
  }, [pos]);
  if (items.length === 0) return null;
  return (
    <>
      <IconButton
        aria-label={label}
        title={label}
        aria-haspopup="menu"
        aria-expanded={pos !== null}
        onClick={(e) => {
          const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
          setPos(pos ? null : { top: r.bottom + 4, right: window.innerWidth - r.right });
        }}
      >
        <MoreVertical size={15} />
      </IconButton>
      {pos && (
        <div role="menu" aria-label={label} className="mvx-actions-menu" style={{ top: pos.top, right: pos.right }}>
          {items.map((item) => (
            <button key={item.label} type="button" role="menuitem" className="mvx-actions-menu__item" disabled={item.disabled}
              onClick={() => { item.onSelect(); setPos(null); }}>
              {item.icon}
              {item.label}
            </button>
          ))}
        </div>
      )}
    </>
  );
}
