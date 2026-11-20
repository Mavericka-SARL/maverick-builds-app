// How many decimals an amount is written with. A value of 1,000 or more —
// thousands, millions, any large amount shown in full — is written whole:
// €20,418,650, not €20,418,650.00 (owner, 2026-11-20). A smaller value keeps
// the decimals its format asks for (a rate 4.94, a price 12.50), and so does
// an amount written in thousands or millions, which is below 1,000 once
// scaled (€20.4M). Percentages are not amounts and keep their decimals.
export const WHOLE_FROM = 1000;

export function shownDecimals(shown: number, decimals: number): number {
  return Math.abs(shown) >= WHOLE_FROM ? 0 : decimals;
}
