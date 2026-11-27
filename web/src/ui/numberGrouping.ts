// Thousands grouping of a number being typed (GroupedNumberInput).

/** The grouping character: a no-break space. */
export const GROUP = "\u00a0";

/** text grouped: "-1234567,5" → "-1 234 567,5". */
export function groupDigits(text: string): string {
  const m = /^(-?)(\d*)([.,]\d*)?$/.exec(text);
  if (!m) return text;
  const [, sign, whole, frac = ""] = m;
  return sign + whole.replace(/\B(?=(\d{3})+(?!\d))/g, GROUP) + frac;
}

/** text without its grouping, as the person typed it otherwise. */
export function ungroup(text: string): string {
  return text.replace(/[\s\u00a0\u202f']/g, "");
}

/** The number text says, null when it says none yet ("", "-", "12,"). */
export function numberOf(text: string): number | null {
  const s = ungroup(text).replace(",", ".");
  if (!/^-?(\d+\.?\d*|\.\d+)$/.test(s)) return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

/** A value as the field shows it, grouped. */
export function textOf(v: number | string | null | undefined): string {
  if (v === "" || v === null || v === undefined) return "";
  const n = typeof v === "number" ? v : Number(String(v).replace(",", "."));
  return Number.isFinite(n) ? groupDigits(String(n)) : String(v);
}
