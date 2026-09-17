import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { LogOut } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { RoleBadge } from "../ui";
import { useAuth } from "../auth/useAuth";
import { api } from "../api/client";
import { EDITION_LABELS, useLicense } from "../license/useLicense";

/**
 * The account control in the top-right corner, beside the notification bell.
 *
 * The shell states who you are and which role you hold in the context bar,
 * where it is always readable. Everything that would repeat that — the address,
 * the full role list — lives in here instead, along with Sign out, so the same
 * three facts are not printed twice on one screen.
 *
 * Sign out is behind a disclosure on purpose: it is one click from ending the
 * session, and a bare icon next to the bell is easy to hit by accident.
 *
 * The panel is portalled to <body> and positioned with fixed coordinates
 * rather than being absolutely positioned next to the trigger. Its container,
 * .mvx-context-bar, sets overflow-x: auto — and a box that clips on one axis
 * clips on both — so an absolutely positioned panel was cut off at the bar's
 * 48px edge. It still had a bounding box, so it looked open to code and to
 * Playwright, but document.elementFromPoint over it returned the page behind:
 * every click went straight through. The bell escapes this by using Drawer,
 * which is its own overlay.
 */
function initialsOf(name: string, email: string): string {
  const source = name.trim() || email.trim();
  if (!source) return "?";
  const words = source.split(/[\s.@_-]+/).filter(Boolean);
  if (words.length >= 2) return (words[0][0] + words[1][0]).toUpperCase();
  return source.slice(0, 2).toUpperCase();
}

export function UserMenu() {
  const { logout } = useAuth();
  const { data: me } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const license = useLicense();
  const [open, setOpen] = useState(false);
  const [style, setStyle] = useState<React.CSSProperties>({});
  const triggerRef = useRef<HTMLButtonElement>(null);
  const popoverRef = useRef<HTMLDivElement>(null);

  const place = useCallback(() => {
    const t = triggerRef.current?.getBoundingClientRect();
    if (!t) return;
    // Right-aligned to the trigger, clamped so a narrow viewport cannot push
    // the panel off the left edge.
    setStyle({ top: t.bottom + 6, left: Math.max(8, t.right - 232) });
  }, []);

  useLayoutEffect(() => {
    if (open) place();
  }, [open, place]);

  useEffect(() => {
    if (!open) return;
    // Fixed coordinates go stale the moment anything moves underneath them.
    const onMove = () => place();
    // Dismissal has to cover both routes out: clicking elsewhere and pressing
    // Escape. The panel lives outside this component's DOM subtree now, so an
    // outside-click test has to consult it directly.
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (triggerRef.current?.contains(target) || popoverRef.current?.contains(target)) return;
      setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    window.addEventListener("resize", onMove);
    window.addEventListener("scroll", onMove, true);
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("resize", onMove);
      window.removeEventListener("scroll", onMove, true);
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open, place]);

  // Read defensively: this sits in the shell of every console, so an
  // unexpected body must not throw during render and take the nav with it.
  const roles = Array.isArray(me?.roles) ? [...new Set(me.roles)] : [];
  const name = me?.display_name ?? "";
  const email = me?.email ?? "";

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        className="mvx-user-menu__trigger"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={name || email ? `Account: ${name || email}` : "Account"}
        onClick={() => setOpen((v) => !v)}
      >
        {initialsOf(name, email)}
      </button>

      {open && createPortal(
        <div ref={popoverRef} className="mvx-user-menu__popover" role="menu" style={style}>
          {(name || email) && (
            <div className="mvx-user-menu__identity">
              {name && <div className="mvx-user-menu__name">{name}</div>}
              {email && <div className="mvx-user-menu__email">{email}</div>}
              {roles.length > 0 && (
                <div className="mvx-user-menu__roles">
                  {roles.map((r) => <RoleBadge key={r} role={r} />)}
                </div>
              )}
              {/* The edition is deployment-wide, not per person, but the
                  account menu is the one place every role opens. */}
              <div className="mvx-user-menu__edition" data-testid="edition">
                {EDITION_LABELS[license.edition]} edition
                {license.state === "expired" ? " · license expired" : license.state === "invalid" ? " · license invalid" : ""}
              </div>
            </div>
          )}
          <button
            type="button"
            role="menuitem"
            className="mvx-user-menu__action"
            onClick={() => {
              setOpen(false);
              logout();
            }}
          >
            <LogOut size={14} aria-hidden="true" />
            Sign out
          </button>
        </div>,
        document.body,
      )}
    </>
  );
}
