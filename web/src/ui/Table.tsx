import React from "react";
import { thStyle, tdStyle } from "./tableStyles";

/** Shared table wrapper for consistent table layout */
export function Table({ children, style }: { children: React.ReactNode; style?: React.CSSProperties }) {
  return (
    <div className="mvx-table-wrap" style={style}>
      <table className="mvx-table">
        {children}
      </table>
    </div>
  );
}

export function Th({ children, style, ...rest }: React.ThHTMLAttributes<HTMLTableCellElement>) {
  return <th style={{ ...thStyle, ...style }} {...rest}>{children}</th>;
}

export function Td({ children, style, ...rest }: React.TdHTMLAttributes<HTMLTableCellElement>) {
  return <td style={{ ...tdStyle, ...style }} {...rest}>{children}</td>;
}

/** Row with consistent height */
export function Tr({ children, style, onClick }: { children: React.ReactNode; style?: React.CSSProperties; onClick?: () => void }) {
  return (
    <tr
      onClick={onClick}
      style={{
        minHeight: 46,
        transition: "background 0.1s",
        cursor: onClick ? "pointer" : undefined,
        ...style,
      }}
      onMouseEnter={onClick ? (e) => { e.currentTarget.style.background = "var(--color-surface-subtle)"; } : undefined}
      onMouseLeave={onClick ? (e) => { e.currentTarget.style.background = ""; } : undefined}
    >
      {children}
    </tr>
  );
}
