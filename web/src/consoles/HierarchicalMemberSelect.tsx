import React, { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { SearchInput } from "../ui";
import { buildMemberTree, type MemberTreeNode } from "./dashboardLayout";

interface HierarchicalMember {
  code: string;
  label: string;
  parent_code?: string;
}

// pruneTree drops any node isSelectable rejects that also has zero
// surviving descendants — e.g. a form field's "allowed_members" subset
// filter, where an intermediate group whose every leaf got excluded
// shouldn't render as a dead-end header. isRawLeaf reflects the node's
// position in the ORIGINAL tree (computed before recursing), not whether
// it happens to have zero children after pruning — a group node that lost
// all its children to filtering is still a group, never treated as if it
// had been a leaf all along.
function pruneTree<T extends HierarchicalMember>(
  nodes: MemberTreeNode<T>[],
  isSelectable: (member: T, isRawLeaf: boolean) => boolean,
): MemberTreeNode<T>[] {
  const out: MemberTreeNode<T>[] = [];
  for (const n of nodes) {
    const isRawLeaf = n.children.length === 0;
    const children = pruneTree(n.children, isSelectable);
    if (isSelectable(n.member, isRawLeaf) || children.length > 0) {
      out.push({ ...n, children });
    }
  }
  return out;
}

// flattenSelectable returns every selectable node (i.e. not a leafOnly
// group header) in the same depth-first order they render, for keyboard
// navigation — the flat sequence Arrow Up/Down step through.
function flattenSelectable<T extends HierarchicalMember>(nodes: MemberTreeNode<T>[], leafOnly: boolean | undefined): MemberTreeNode<T>[] {
  const out: MemberTreeNode<T>[] = [];
  for (const n of nodes) {
    if (!(leafOnly && n.children.length > 0)) out.push(n);
    if (n.children.length > 0) out.push(...flattenSelectable(n.children, leafOnly));
  }
  return out;
}

// filterTree keeps a node when it or any descendant matches the query, and
// independently re-filters each surviving node's own children the same
// way — a match's ancestor chain stays visible for context, but a
// non-matching sibling branch under a matching ancestor is still hidden.
// Mirrors DimensionsTab.tsx's flattenVisible search semantics, adapted
// from its parent_member_id-keyed tree to this parent_code-keyed one.
function filterTree<T extends HierarchicalMember>(nodes: MemberTreeNode<T>[], query: string): MemberTreeNode<T>[] {
  if (!query) return nodes;
  const q = query.toLowerCase();
  const out: MemberTreeNode<T>[] = [];
  for (const n of nodes) {
    const children = filterTree(n.children, query);
    const selfMatch = n.member.label.toLowerCase().includes(q) || n.member.code.toLowerCase().includes(q);
    if (selfMatch || children.length > 0) {
      out.push({ ...n, children });
    }
  }
  return out;
}

export function HierarchicalMemberSelect<T extends HierarchicalMember>({
  id,
  ariaLabel,
  members,
  value,
  onChange,
  disabled,
  className,
  onMouseDown,
  placeholder,
  leafOnly,
  isSelectable,
}: {
  id?: string;
  ariaLabel: string;
  members: T[];
  value: string;
  onChange: (code: string) => void;
  disabled?: boolean;
  className?: string;
  onMouseDown?: (e: React.MouseEvent) => void;
  /** Shown on the trigger when `value` matches no member (e.g. unset). */
  placeholder?: string;
  /** Only leaf (childless) members are selectable — non-leaf nodes render
   *  as non-interactive group headers. For data-entry fields (e.g. a form
   *  picking a specific department) rather than filter/context selectors,
   *  where picking an aggregate/rollup node is a legitimate choice. */
  leafOnly?: boolean;
  /** Additional per-member filter — a leaf failing this is excluded from
   *  the tree entirely (not just disabled), e.g. a form field's
   *  `allowed_members` subset. Never applied to non-leaf nodes directly
   *  (those are governed by `leafOnly` + whether they retain any
   *  surviving descendant). */
  isSelectable?: (member: T) => boolean;
}) {
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [activeCode, setActiveCode] = useState<string | null>(null);
  const [popoverStyle, setPopoverStyle] = useState<React.CSSProperties>({});
  const triggerRef = useRef<HTMLButtonElement>(null);
  const popoverRef = useRef<HTMLDivElement>(null);
  const listboxId = useId();
  const optionId = useCallback((code: string) => `${listboxId}-opt-${code}`, [listboxId]);

  const selected = members.find(m => m.code === value);
  let tree = buildMemberTree(members);
  if (leafOnly || isSelectable) {
    tree = pruneTree(tree, (member, isRawLeaf) => {
      if (leafOnly && !isRawLeaf) return false;
      if (isSelectable && !isSelectable(member)) return false;
      return true;
    });
  }
  const visibleTree = filterTree(tree, search);
  const flatOptions = flattenSelectable(visibleTree, leafOnly);
  // Ref mirrors so the keydown listener (registered once per `open` toggle,
  // not re-registered on every keystroke) always reads the current, not
  // stale-at-registration-time, option list/active code — search narrows
  // the list live. Refs can't be written during render, so the mirroring
  // itself happens in the deps-less effect below (runs after every commit).
  const flatOptionsRef = useRef(flatOptions);
  const activeCodeRef = useRef(activeCode);
  useEffect(() => {
    flatOptionsRef.current = flatOptions;
    activeCodeRef.current = activeCode;
  });

  const pick = useCallback((code: string) => {
    onChange(code);
    setOpen(false);
    setSearch("");
    triggerRef.current?.focus();
  }, [onChange]);

  // Keep the keyboard-active option valid as the visible set changes (open,
  // or search narrows/widens it): prefer the current value if still
  // present, else the first visible option, else nothing.
  useEffect(() => {
    if (!open) return;
    setActiveCode(prev => { // eslint-disable-line react-hooks/set-state-in-effect -- syncing keyboard-active selection to an external trigger (open/search changing the visible option set), not derivable from props alone
      if (prev && flatOptions.some(n => n.member.code === prev)) return prev;
      if (flatOptions.some(n => n.member.code === value)) return value;
      return flatOptions[0]?.member.code ?? null;
    });
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, search]);

  function reposition() {
    const rect = triggerRef.current?.getBoundingClientRect();
    if (!rect) return;
    setPopoverStyle({
      position: "fixed",
      top: rect.bottom + 4,
      left: rect.left,
      minWidth: rect.width,
      // Above the Dialog overlay (zIndex 1000): without this, a select
      // inside a dialog paints its popover UNDER the overlay — the options
      // are visible through it but every click lands on the overlay, so
      // nothing is selectable (found live in the start-workflow dialog).
      zIndex: 1100,
    });
  }

  // useLayoutEffect (not useEffect): must position the portal BEFORE the
  // browser paints, or the search input's focus() below (even called here,
  // in the same layout effect) sees an unstyled/unpositioned popover still
  // sitting at its default static position at the end of document.body and
  // scrolls the whole page to bring THAT into view, instead of the
  // trigger's actual on-screen location.
  useLayoutEffect(() => {
    if (!open) return;
    reposition();
    popoverRef.current?.querySelector<HTMLInputElement>("input")?.focus({ preventScroll: true });
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onScroll = () => reposition();
    const onResize = () => reposition();
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onResize);
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") { setOpen(false); triggerRef.current?.focus(); return; }
      const opts = flatOptionsRef.current;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        if (opts.length === 0) return;
        e.preventDefault();
        setActiveCode(prev => {
          const i = opts.findIndex(n => n.member.code === prev);
          const next = e.key === "ArrowDown"
            ? Math.min(opts.length - 1, i + 1)
            : Math.max(0, i === -1 ? 0 : i - 1);
          return opts[next]?.member.code ?? prev;
        });
        return;
      }
      if (e.key === "Home" || e.key === "End") {
        if (opts.length === 0) return;
        e.preventDefault();
        setActiveCode((e.key === "Home" ? opts[0] : opts[opts.length - 1]).member.code);
        return;
      }
      if (e.key === "Enter") {
        e.preventDefault();
        if (activeCodeRef.current !== null) pick(activeCodeRef.current);
      }
    };
    const onMouseDownOutside = (e: MouseEvent) => {
      const target = e.target as Node;
      if (triggerRef.current?.contains(target) || popoverRef.current?.contains(target)) return;
      setOpen(false);
    };
    window.addEventListener("keydown", onKeyDown);
    document.addEventListener("mousedown", onMouseDownOutside);
    return () => {
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onResize);
      window.removeEventListener("keydown", onKeyDown);
      document.removeEventListener("mousedown", onMouseDownOutside);
    };
  }, [open, pick]);

  // Keep the keyboard-active option visible when Arrow/Home/End moves it
  // past the popover's scrollable tree area.
  useEffect(() => {
    if (!open || !activeCode) return;
    popoverRef.current?.querySelector(`#${CSS.escape(optionId(activeCode))}`)?.scrollIntoView({ block: "nearest" });
  }, [activeCode, open, optionId]);

  function renderNodes(nodes: MemberTreeNode<T>[]): React.ReactNode {
    return nodes.map(n => {
      const isGroup = leafOnly && n.children.length > 0;
      return (
        <React.Fragment key={n.member.code}>
          <div
            id={isGroup ? undefined : optionId(n.member.code)}
            role={isGroup ? undefined : "option"}
            aria-selected={isGroup ? undefined : n.member.code === value}
            aria-disabled={isGroup ? true : undefined}
            className={[
              "mvx-hier-select__option",
              isGroup ? "mvx-hier-select__option--group" : "",
              !isGroup && n.member.code === value ? "mvx-hier-select__option--selected" : "",
              !isGroup && n.member.code === activeCode ? "mvx-hier-select__option--active" : "",
            ].filter(Boolean).join(" ")}
            style={{ paddingLeft: 10 + n.level * 16 }}
            onClick={isGroup ? undefined : () => pick(n.member.code)}
            onMouseEnter={isGroup ? undefined : () => setActiveCode(n.member.code)}
          >
            {n.member.label}
          </div>
          {n.children.length > 0 && renderNodes(n.children)}
        </React.Fragment>
      );
    });
  }

  return (
    <>
      <button
        ref={triggerRef}
        id={id}
        type="button"
        disabled={disabled}
        className={["mvx-hier-select__trigger", className].filter(Boolean).join(" ")}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={listboxId}
        aria-label={ariaLabel}
        onMouseDown={onMouseDown}
        onClick={() => setOpen(o => !o)}
      >
        <span className="mvx-hier-select__trigger-label">{selected?.label ?? (value ? value : placeholder ?? "")}</span>
      </button>
      {open && createPortal(
        <div ref={popoverRef} role="listbox" id={listboxId} className="mvx-hier-select__popover" style={popoverStyle}>
          <div className="mvx-hier-select__search">
            <SearchInput
              value={search}
              onChange={e => setSearch(e.target.value)}
              placeholder="Search…"
              width="100%"
              role="combobox"
              aria-expanded={open}
              aria-controls={listboxId}
              aria-activedescendant={activeCode ? optionId(activeCode) : undefined}
            />
          </div>
          <div className="mvx-hier-select__tree">
            {visibleTree.length > 0 ? renderNodes(visibleTree) : <div className="mvx-hier-select__empty">No matches</div>}
          </div>
        </div>,
        document.body,
      )}
    </>
  );
}
