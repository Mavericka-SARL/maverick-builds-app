// A date-format metric's cell holds the serial number DATE() gives — days
// since 1899-12-30, the spreadsheet's — sent and stored as yyyy-mm-dd, shown
// in its model's date format or, with none, as the reader's browser writes a
// date.
const EPOCH = Date.UTC(1899, 11, 30);
const DAY = 86_400_000;

export function serialToISO(serial: number): string {
  return new Date(EPOCH + Math.round(serial * DAY)).toISOString().slice(0, 10);
}

// A date as its model writes it (format: the model's date format, see
// formatIsoDate), else as the reader's browser writes it in its date inputs
// (01/07/2026 in en-GB, 07/01/2026 in en-US), so a date reads the same typed
// or shown; read only, a cell showed 2026-07-01 beside an input showing
// 01/07/2026.
export function isoToLocal(iso: string, format = ""): string {
  if (format) return formatIsoDate(iso, format);
  const d = new Date(`${iso.slice(0, 10)}T00:00:00Z`);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString(undefined, { timeZone: "UTC", day: "2-digit", month: "2-digit", year: "numeric" });
}

export function serialToLocal(serial: number, format = ""): string {
  return isoToLocal(serialToISO(serial), format);
}

// A model's date formats (modeledit.DateFormats): how its readers see and
// type dates. "" leaves it to each reader's browser.
export const DATE_FORMATS = ["DD/MM/YYYY", "MM/DD/YYYY", "YYYY-MM-DD", "DD.MM.YYYY", "D MMM YYYY"] as const;
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/** yyyy-mm-dd written in format; as given when it is no date. */
export function formatIsoDate(iso: string, format: string): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(iso);
  if (!m) return iso;
  const [y, mo, d] = [m[1], m[2], m[3]];
  switch (format) {
    case "DD/MM/YYYY": return `${d}/${mo}/${y}`;
    case "MM/DD/YYYY": return `${mo}/${d}/${y}`;
    case "DD.MM.YYYY": return `${d}.${mo}.${y}`;
    case "D MMM YYYY": return `${Number(d)} ${MONTHS[Number(mo) - 1]} ${y}`;
    default: return `${y}-${mo}-${d}`;
  }
}

/** A typed date in format (yyyy-mm-dd is always read too) as yyyy-mm-dd; null when it is no date. */
export function parseDateText(text: string, format: string): string | null {
  const s = text.trim();
  let y: number, mo: number, d: number;
  const iso = /^(\d{4})-(\d{1,2})-(\d{1,2})$/.exec(s);
  const num = /^(\d{1,2})[./-](\d{1,2})[./-](\d{4})$/.exec(s);
  const named = /^(\d{1,2})\s+([A-Za-z]{3,})\.?,?\s+(\d{4})$/.exec(s);
  if (iso) {
    [y, mo, d] = [Number(iso[1]), Number(iso[2]), Number(iso[3])];
  } else if (num) {
    const [a, b] = [Number(num[1]), Number(num[2])];
    [d, mo] = format === "MM/DD/YYYY" ? [b, a] : [a, b];
    y = Number(num[3]);
  } else if (named) {
    d = Number(named[1]);
    mo = MONTHS.findIndex(x => named[2].toLowerCase().startsWith(x.toLowerCase())) + 1;
    y = Number(named[3]);
  } else {
    return null;
  }
  const date = new Date(Date.UTC(y, mo - 1, d));
  if (y < 1000 || mo < 1 || date.getUTCFullYear() !== y || date.getUTCMonth() !== mo - 1 || date.getUTCDate() !== d) return null;
  return `${y}-${String(mo).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
}

export function isoToSerial(iso: string): number {
  const [y, m, d] = iso.split("-").map(Number);
  return Math.round((Date.UTC(y, m - 1, d) - EPOCH) / DAY);
}
