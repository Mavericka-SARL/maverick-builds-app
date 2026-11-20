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

// The most decimals a widget's own Decimals setting offers.
export const MAX_WIDGET_DECIMALS = 6;

// A widget's Decimals setting (widget_props.decimals) wins over the rule
// above and over its metric's decimals: the developer asked for exactly that
// many on every amount the widget shows (owner, 2026-11-20). Absent or out of
// range, the rule applies.
export function amountDecimals(shown: number, decimals: number, fixed?: number | null): number {
  return widgetDecimals(fixed) ?? shownDecimals(shown, decimals);
}

// widgetDecimals is the widget's setting when it is a whole number from 0 to
// MAX_WIDGET_DECIMALS, else undefined (automatic).
export function widgetDecimals(fixed?: number | null): number | undefined {
  return typeof fixed === "number" && Number.isInteger(fixed) && fixed >= 0 && fixed <= MAX_WIDGET_DECIMALS ? fixed : undefined;
}
