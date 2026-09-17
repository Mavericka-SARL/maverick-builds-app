import type React from "react";
import { X } from "lucide-react";

interface ToolbarProps {
  children: React.ReactNode;
  className?: string;
}

interface ToolbarGroupProps {
  children: React.ReactNode;
  align?: "start" | "end";
  className?: string;
}

interface FilterChipProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  active?: boolean;
  onClear?: () => void;
}

export function Toolbar({ children, className }: ToolbarProps) {
  return <div className={["mvx-toolbar", className].filter(Boolean).join(" ")}>{children}</div>;
}

export function ToolbarGroup({ children, align = "start", className }: ToolbarGroupProps) {
  return (
    <div
      className={["mvx-toolbar__group", align === "end" ? "mvx-toolbar__group--end" : "", className].filter(Boolean).join(" ")}
    >
      {children}
    </div>
  );
}

export function FilterBar({ children, className }: ToolbarProps) {
  return <div className={["mvx-filter-bar", className].filter(Boolean).join(" ")}>{children}</div>;
}

export function FilterBarGroup({ children, className }: Omit<ToolbarGroupProps, "align">) {
  return <div className={["mvx-filter-bar__group", className].filter(Boolean).join(" ")}>{children}</div>;
}

export function FilterChip({ active = false, onClear, children, className, ...rest }: FilterChipProps) {
  return (
    <button
      type="button"
      {...rest}
      className={["mvx-filter-chip", active ? "mvx-filter-chip--active" : "", className].filter(Boolean).join(" ")}
    >
      {children}
      {onClear && (
        <X
          size={12}
          aria-hidden="true"
          onClick={(event) => {
            event.stopPropagation();
            onClear();
          }}
        />
      )}
    </button>
  );
}

export type { ToolbarGroupProps, ToolbarProps };
