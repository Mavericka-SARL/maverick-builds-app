// A date-format metric's cell holds the serial number DATE() gives — days
// since 1899-12-30, the spreadsheet's — sent and stored as yyyy-mm-dd, shown
// as the reader's browser writes a date.
const EPOCH = Date.UTC(1899, 11, 30);
const DAY = 86_400_000;

export function serialToISO(serial: number): string {
  return new Date(EPOCH + Math.round(serial * DAY)).toISOString().slice(0, 10);
}

// A date as the reader's browser writes it in its date inputs (01/07/2026 in
// en-GB, 07/01/2026 in en-US), so a date reads the same typed or shown; read
// only, a cell showed 2026-07-01 beside an input showing 01/07/2026.
export function isoToLocal(iso: string): string {
  const d = new Date(`${iso.slice(0, 10)}T00:00:00Z`);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString(undefined, { timeZone: "UTC", day: "2-digit", month: "2-digit", year: "numeric" });
}

export function serialToLocal(serial: number): string {
  return isoToLocal(serialToISO(serial));
}

export function isoToSerial(iso: string): number {
  const [y, m, d] = iso.split("-").map(Number);
  return Math.round((Date.UTC(y, m - 1, d) - EPOCH) / DAY);
}
