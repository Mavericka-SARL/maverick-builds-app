import { ChevronDown, ChevronRight } from "lucide-react";
import type React from "react";
import { IconButton } from "./IconButton";
import type { DataTableColumn } from "./DataTable";

export interface TreeTableRow<T> {
  id: string;
  item: T;
  level: number;
  expandable?: boolean;
  expanded?: boolean;
}

interface TreeTableProps<T> {
  columns: [DataTableColumn<T>, ...DataTableColumn<T>[]];
  rows: TreeTableRow<T>[];
  onToggle?: (row: TreeTableRow<T>) => void;
  actions?: (row: TreeTableRow<T>) => React.ReactNode;
  emptyTitle?: React.ReactNode;
  density?: "default" | "compact";
}

export function TreeTable<T>({
  columns,
  rows,
  onToggle,
  actions,
  emptyTitle = "No hierarchy members",
  density = "default",
}: TreeTableProps<T>) {
  const [first, ...rest] = columns;
  const colSpan = columns.length + (actions ? 1 : 0);

  return (
    <div className="mvx-table-wrap">
      <table className={["mvx-table", density === "compact" ? "mvx-table--compact" : ""].filter(Boolean).join(" ")}>
        <thead>
          <tr>
            {columns.map((column) => (
              <th key={column.id} style={{ width: column.width, textAlign: column.align }}>
                {column.header}
              </th>
            ))}
            {actions && <th style={{ width: 1, textAlign: "right" }}>Actions</th>}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <td colSpan={colSpan}>
                <div className="mvx-table__empty">{emptyTitle}</div>
              </td>
            </tr>
          )}
          {rows.map((row) => (
            <tr key={row.id}>
              <td style={{ textAlign: first.align }} className={first.className}>
                <div style={{ display: "flex", alignItems: "center", gap: 4, paddingLeft: row.level * 18 }}>
                  {row.expandable ? (
                    <IconButton
                      aria-label={row.expanded ? "Collapse row" : "Expand row"}
                      size={24}
                      onClick={() => onToggle?.(row)}
                    >
                      {row.expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                    </IconButton>
                  ) : (
                    <span style={{ width: 24, height: 24, flexShrink: 0 }} />
                  )}
                  <span>{first.cell(row.item)}</span>
                </div>
              </td>
              {rest.map((column) => (
                <td key={column.id} style={{ textAlign: column.align }} className={column.className}>
                  {column.cell(row.item)}
                </td>
              ))}
              {actions && <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>{actions(row)}</td>}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export type { TreeTableProps };
