// Copying and pasting a grid's cells, as a spreadsheet does: a block of
// cells travels as tab-separated lines, so it goes to and from Excel or
// Google Sheets, and between grids. Every editable cell of a grid carries
// what a paste needs (data-cell, data-kind, data-metric, data-codes and its
// raw value in data-raw); these helpers read the table, not React state.

/** The rows and columns of copied text: lines of tab-separated values. */
export function parseClipboard(text: string): string[][] {
  const lines = text.replace(/\r\n?/g, "\n").replace(/\n$/, "").split("\n");
  return lines.map(line => line.split("\t"));
}

/** A block of values as the clipboard carries it. */
export function toClipboard(rows: string[][]): string {
  return rows.map(r => r.join("\t")).join("\n");
}

/** Where a cell stands in its table body: row and column. */
export function cellPosition(el: Element): { body: HTMLTableSectionElement; r: number; c: number } | null {
  const td = el.closest("td");
  const tr = td?.parentElement as HTMLTableRowElement | null;
  const body = tr?.parentElement as HTMLTableSectionElement | null;
  if (!td || !tr || !body) return null;
  return { body, r: tr.sectionRowIndex, c: td.cellIndex };
}

/** The table cells of the rectangle between two cells of one body, row by row. */
export function cellsBetween(a: Element, b: Element): HTMLTableCellElement[][] {
  const pa = cellPosition(a), pb = cellPosition(b);
  if (!pa || !pb || pa.body !== pb.body) return [];
  const rows: HTMLTableCellElement[][] = [];
  for (let r = Math.min(pa.r, pb.r); r <= Math.max(pa.r, pb.r); r++) {
    const tr = pa.body.rows[r];
    if (!tr) continue;
    const row: HTMLTableCellElement[] = [];
    for (let c = Math.min(pa.c, pb.c); c <= Math.max(pa.c, pb.c); c++) {
      const td = tr.cells[c];
      if (td) row.push(td);
    }
    rows.push(row);
  }
  return rows;
}

/** The table cell r rows down and c columns right of a cell, if the body has one. */
export function cellAt(from: Element, r: number, c: number): HTMLTableCellElement | null {
  const p = cellPosition(from);
  if (!p) return null;
  return p.body.rows[p.r + r]?.cells[p.c + c] ?? null;
}

/** The editable cell inside a table cell, if it has one. */
export function editableIn(td: Element | null): HTMLElement | null {
  return (td?.querySelector("[data-cell]") as HTMLElement | null) ?? null;
}

/** What a cell copies as: an editable cell its raw value, any other what it shows. */
export function copyText(td: HTMLTableCellElement): string {
  const el = editableIn(td);
  if (el) return el.dataset.raw ?? "";
  return (td.innerText ?? "").replace(/\s*\n\s*/g, " ").trim();
}

// The order a reader's locale writes day and month in ("20/09/2027" in
// en-GB, "09/20/2027" in en-US), for a pasted date in that form.
function dayFirst(): boolean {
  const parts = new Intl.DateTimeFormat(undefined, { day: "2-digit", month: "2-digit", year: "numeric" }).formatToParts(new Date(Date.UTC(2027, 8, 20)));
  return parts.findIndex(p => p.type === "day") < parts.findIndex(p => p.type === "month");
}

/** A pasted date as yyyy-mm-dd: written so, or in the reader's day/month order; null when it is no date. */
export function pastedDate(raw: string): string | null {
  const s = raw.trim();
  let y: number, m: number, d: number;
  const iso = /^(\d{4})-(\d{1,2})-(\d{1,2})$/.exec(s);
  const local = /^(\d{1,2})[./-](\d{1,2})[./-](\d{4})$/.exec(s);
  if (iso) {
    [y, m, d] = [Number(iso[1]), Number(iso[2]), Number(iso[3])];
  } else if (local) {
    const [first, second] = [Number(local[1]), Number(local[2])];
    [d, m] = dayFirst() ? [first, second] : [second, first];
    y = Number(local[3]);
  } else {
    return null;
  }
  const date = new Date(Date.UTC(y, m - 1, d));
  if (y < 1000 || date.getUTCFullYear() !== y || date.getUTCMonth() !== m - 1 || date.getUTCDate() !== d) return null;
  return `${y}-${String(m).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
}
