/**
 * A number typed into a grid cell, read the way people write one. The cell
 * shows its value formatted ("$14.69", "5.60%"), so an edit in place keeps the
 * symbol; and many people type a decimal comma. Reported live (2026-10-18):
 * "$14.80" saved nothing, silently, and "67,40" became 6740.
 *
 * - currency signs, "%" and spaces (also no-break and thin ones, and the
 *   apostrophe some write thousands with) are ignored: "$14.80", "12 %";
 * - "(2)" is −2, as accounts write a reduction; a typographic minus counts;
 * - with both "." and ",", the last one is the decimal point: "1,234.5",
 *   "1.234,5";
 * - with only ",": one comma not followed by exactly three digits is a decimal
 *   comma ("67,40", "1,5"); otherwise commas group thousands ("1,234",
 *   "12,345,678");
 * - with only ".": several dots group thousands ("1.234.567").
 *
 * Returns null when what is left is not a number.
 */
export function parseCellNumber(raw: string): number | null {
  let s = raw.trim().replace(/[\s\u00a0\u202f'\u2019]/g, "");
  let negative = false;
  if (/^\(.*\)$/.test(s)) {
    negative = true;
    s = s.slice(1, -1);
  }
  s = s.replace(/[\u2212\u2012\u2013]/g, "-").replace(/[$€£¥₽%]/g, "");
  const lastDot = s.lastIndexOf("."), lastComma = s.lastIndexOf(",");
  if (lastDot >= 0 && lastComma >= 0) {
    const decimal = lastDot > lastComma ? "." : ",";
    s = s.split(decimal === "." ? "," : ".").join("").replace(",", ".");
  } else if (lastComma >= 0) {
    const parts = s.split(",");
    s = parts.length === 2 && parts[1].length !== 3 ? parts.join(".") : parts.join("");
  } else if ((s.match(/\./g) ?? []).length > 1) {
    s = s.split(".").join("");
  }
  if (!/^[-+]?(\d+\.?\d*|\.\d+)(e[-+]?\d+)?$/i.test(s)) return null;
  const n = Number(s);
  if (!Number.isFinite(n)) return null;
  return negative ? -n : n;
}
