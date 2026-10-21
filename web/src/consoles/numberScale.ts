import type { NumberScale } from "../api/client";

// How an amount is written at a widget's number_scale (IBCS: one unit per
// table or tile, said once). "k" and "m" divide every amount alike — a
// grid of 26 monthly columns reads 1,061.6 under "thousands" instead of
// 1,061,621 — and "auto", a tile's default, shortens millions and billions
// (€20.1M, not €20,098,192) and leaves a smaller amount whole ($90,000 reads
// at a glance). Only numbers and currency amounts scale; a percentage, a
// date or a count of days never does.

export interface Scale {
  div: number;
  /** Written after the number on a tile ("M"); a grid says the unit once, in its caption. */
  suffix: string;
  /** The unit as a reader says it: "thousands". */
  unit: string;
  /** Decimal places at this scale, else the metric's own. */
  decimals?: number;
}

const NONE: Scale = { div: 1, suffix: "", unit: "" };

export function scaleFor(scale: NumberScale | undefined, value?: number): Scale {
  switch (scale) {
    case "k":
      return { div: 1e3, suffix: "k", unit: "thousands", decimals: 1 };
    case "m":
      return { div: 1e6, suffix: "M", unit: "millions", decimals: 1 };
    case "auto": {
      const a = Math.abs(value ?? 0);
      if (a >= 1e9) return { div: 1e9, suffix: "B", unit: "billions", decimals: 1 };
      if (a >= 1e6) return { div: 1e6, suffix: "M", unit: "millions", decimals: 1 };
      return NONE;
    }
    default:
      return NONE;
  }
}

/** Whether amounts of this metric format scale. */
export function scales(format: string | undefined): boolean {
  return !format || format === "number" || format === "currency";
}
