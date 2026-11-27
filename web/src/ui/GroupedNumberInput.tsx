import { useLayoutEffect, useRef, useState } from "react";
import { GROUP, groupDigits, numberOf, textOf, ungroup } from "./numberGrouping";

/**
 * A number typed with its thousands shown as it is typed — 5 000 000, not
 * 5000000 (owner, 2026-11-27). The digits are grouped by threes with a
 * no-break space; a decimal point or comma and a leading minus are kept as
 * typed; the caret stays after the digit it was after. The value handed on
 * is the number ("" for an empty field), so what is saved is unchanged.
 */
export function GroupedNumberInput({ value, onChange, decimals, ...rest }: {
  value: number | string | null | undefined;
  onChange: (v: number | "") => void;
  /** At most this many decimals may be typed (money: 2). */
  decimals?: number;
} & Omit<React.InputHTMLAttributes<HTMLInputElement>, "value" | "onChange" | "type">) {
  const [text, setText] = useState(() => textOf(value));
  const [shownFor, setShownFor] = useState(value);
  const ref = useRef<HTMLInputElement>(null);
  const caret = useRef<number | null>(null);
  // A value set from outside (a record opened, a reset) is shown grouped.
  if (value !== shownFor) {
    setShownFor(value);
    const own = numberOf(text);
    if (!(typeof value === "number" && own === value) && !(value === "" && text === "")) setText(textOf(value));
  }
  useLayoutEffect(() => {
    if (caret.current !== null && ref.current && document.activeElement === ref.current) {
      ref.current.setSelectionRange(caret.current, caret.current);
      caret.current = null;
    }
  });
  return (
    <input
      {...rest}
      className={["mvx-input", rest.className].filter(Boolean).join(" ")}
      ref={ref}
      type="text"
      inputMode="decimal"
      value={text}
      onChange={e => {
        const raw = e.target.value;
        const at = e.target.selectionStart ?? raw.length;
        // The characters before the caret that are not grouping: where it goes.
        const before = ungroup(raw.slice(0, at)).length;
        let plain = ungroup(raw);
        if (!/^-?\d*([.,]\d*)?$/.test(plain)) return; // not a number being typed
        if (decimals !== undefined) {
          const sep = plain.search(/[.,]/);
          if (sep >= 0 && plain.length - sep - 1 > decimals) plain = plain.slice(0, sep + 1 + decimals);
        }
        const next = groupDigits(plain);
        let seen = 0, pos = 0;
        while (pos < next.length && seen < before) {
          if (next[pos] !== GROUP) seen++;
          pos++;
        }
        caret.current = pos;
        setText(next);
        const n = numberOf(next);
        const nextValue = plain === "" ? "" : n ?? "";
        setShownFor(nextValue);
        onChange(nextValue);
      }}
    />
  );
}
