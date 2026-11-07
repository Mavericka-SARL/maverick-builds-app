// A block copied from a spreadsheet reaches the clipboard as tab-separated
// text, a cell holding a line break or a quote wrapped in double quotes. The
// AI Developer imports files and would have to retype a table sent as chat
// text, so the chat attaches such a block as a CSV (see AIAssistant's paste).

export interface PastedTable {
  csv: string;
  rows: number;
  columns: number;
}

function csvCell(value: string): string {
  return /[",\r\n]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
}

/** The table in `text`, or null when it is not one: at least two rows and two
 *  columns, every row as wide as the first. Anything else stays chat text. */
export function parsePastedTable(text: string): PastedTable | null {
  if (!text.includes("\t")) return null;
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let quoted = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quoted) {
      if (c !== '"') cell += c;
      else if (text[i + 1] === '"') { cell += '"'; i++; }
      else quoted = false;
    } else if (c === '"' && cell === "") {
      quoted = true;
    } else if (c === "\t") {
      row.push(cell);
      cell = "";
    } else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++;
      row.push(cell);
      rows.push(row);
      row = [];
      cell = "";
    } else {
      cell += c;
    }
  }
  if (quoted) return null;
  if (cell !== "" || row.length > 0) {
    row.push(cell);
    rows.push(row);
  }
  while (rows.length > 0 && rows[rows.length - 1].every(v => v === "")) rows.pop();
  const columns = rows[0]?.length ?? 0;
  if (rows.length < 2 || columns < 2 || rows.some(r => r.length !== columns)) return null;
  return { csv: rows.map(r => r.map(csvCell).join(",")).join("\r\n") + "\r\n", rows: rows.length, columns };
}
