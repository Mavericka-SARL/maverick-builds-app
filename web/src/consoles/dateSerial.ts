// A date-format metric's cell holds the serial number DATE() gives — days
// since 1899-12-30, the spreadsheet's — shown and typed as yyyy-mm-dd.
const EPOCH = Date.UTC(1899, 11, 30);
const DAY = 86_400_000;

export function serialToISO(serial: number): string {
  return new Date(EPOCH + Math.round(serial * DAY)).toISOString().slice(0, 10);
}

export function isoToSerial(iso: string): number {
  const [y, m, d] = iso.split("-").map(Number);
  return Math.round((Date.UTC(y, m - 1, d) - EPOCH) / DAY);
}
