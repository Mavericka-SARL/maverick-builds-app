import type React from "react";

export interface DataTableColumn<T> {
  id: string;
  header: React.ReactNode;
  cell: (row: T) => React.ReactNode;
  width?: number | string;
  align?: "left" | "center" | "right";
  className?: string;
}

interface DataTableProps<T> {
  columns: DataTableColumn<T>[];
  rows: T[];
  getRowKey: (row: T) => string;
  loading?: boolean;
  emptyTitle?: React.ReactNode;
  emptyDescription?: React.ReactNode;
  onRowClick?: (row: T) => void;
  actions?: (row: T) => React.ReactNode;
  density?: "default" | "compact";
  className?: string;
}

export function DataTable<T>({
  columns,
  rows,
  getRowKey,
  loading = false,
  emptyTitle = "No records",
  emptyDescription,
  onRowClick,
  actions,
  density = "default",
  className,
}: DataTableProps<T>) {
  const colSpan = columns.length + (actions ? 1 : 0);

  return (
    <div className="mvx-table-wrap">
      <table className={["mvx-table", density === "compact" ? "mvx-table--compact" : "", className].filter(Boolean).join(" ")}>
        <thead>
          <tr>
            {columns.map((column) => (
              <th
                key={column.id}
                style={{ width: column.width, textAlign: column.align }}
                className={column.className}
              >
                {column.header}
              </th>
            ))}
            {actions && <th style={{ width: 1, textAlign: "right" }}>Actions</th>}
          </tr>
        </thead>
        <tbody>
          {loading && (
            <tr>
              <td colSpan={colSpan}>
                <div className="mvx-table__loading">Loading...</div>
              </td>
            </tr>
          )}
          {!loading && rows.length === 0 && (
            <tr>
              <td colSpan={colSpan}>
                <div className="mvx-table__empty">
                  <div>{emptyTitle}</div>
                  {emptyDescription && <div style={{ marginTop: 4 }}>{emptyDescription}</div>}
                </div>
              </td>
            </tr>
          )}
          {!loading && rows.map((row) => (
            <tr
              key={getRowKey(row)}
              onClick={onRowClick ? () => onRowClick(row) : undefined}
              style={onRowClick ? { cursor: "pointer" } : undefined}
            >
              {columns.map((column) => (
                <td key={column.id} style={{ textAlign: column.align }} className={column.className}>
                  {column.cell(row)}
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

export type { DataTableProps };
