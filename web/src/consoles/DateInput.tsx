import { useRef, useState } from "react";
import { Calendar } from "lucide-react";
import { formatIsoDate, parseDateText } from "./dateSerial";

// A date field in the model's date format (dateFormat.ts). The browser's own
// date field writes the browser's format (US English: 03/05/2026 for
// 5 March) and cannot be told otherwise; with a model format this one is
// typed in that format, and its calendar button opens the browser's picker.
// Without one it is the browser's field. Either way what is typed stays the
// field's own until it is left or Enter pressed: onCommit gets yyyy-mm-dd,
// or "" when emptied; a date not yet whole is not committed.
export function DateInput({ value, format, onCommit, className, style, ariaLabel, ...rest }: {
  value: string; // yyyy-mm-dd or ""
  format: string;
  onCommit: (iso: string) => void;
  className?: string;
  style?: React.CSSProperties;
  ariaLabel?: string;
} & Record<`data-${string}`, string>) {
  const [draft, setDraft] = useState<string | null>(null);
  const [bad, setBad] = useState(false);
  const picker = useRef<HTMLInputElement>(null);
  if (!format) {
    const commit = () => {
      if (draft === null) return;
      setDraft(null);
      if (draft === value) return;
      if (draft === "" || (/^\d{4}-\d{2}-\d{2}$/.test(draft) && Number(draft.slice(0, 4)) >= 1000)) onCommit(draft);
    };
    return (
      <input type="date" className={className} style={style} aria-label={ariaLabel} {...rest}
        value={draft ?? value} onChange={e => setDraft(e.target.value)} onBlur={commit}
        onKeyDown={e => e.key === "Enter" && commit()} />
    );
  }
  const shown = draft ?? (value ? formatIsoDate(value, format) : "");
  const commit = () => {
    if (draft === null) return;
    const text = draft.trim();
    setDraft(null);
    if (text === "") { if (value !== "") onCommit(""); setBad(false); return; }
    const iso = parseDateText(text, format);
    if (!iso) { setBad(true); return; }
    setBad(false);
    if (iso !== value) onCommit(iso);
  };
  return (
    <span className="mvx-date-input" style={style}>
      <input type="text" className={className} aria-label={ariaLabel} {...rest} placeholder={format}
        aria-invalid={bad || undefined} title={bad ? `Type the date as ${format}` : undefined}
        value={shown} onChange={e => { setDraft(e.target.value); setBad(false); }} onBlur={commit}
        onKeyDown={e => e.key === "Enter" && commit()} />
      <button type="button" className="mvx-date-input__pick" aria-label="Choose a date" tabIndex={-1}
        onClick={() => { const el = picker.current; if (!el) return; el.value = value; try { el.showPicker(); } catch { el.focus(); } }}>
        <Calendar size={13} />
      </button>
      <input ref={picker} type="date" tabIndex={-1} aria-hidden="true" className="mvx-date-input__native"
        onChange={e => { if (e.target.value) { setDraft(null); setBad(false); onCommit(e.target.value); } }} />
    </span>
  );
}
