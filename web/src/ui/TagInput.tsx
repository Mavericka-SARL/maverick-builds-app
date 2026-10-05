import { useState } from "react";
import { FilterChip } from "./Toolbar";
import { TextInput } from "./TextInput";

interface TagInputProps {
  value: string[];
  onChange: (tags: string[]) => void;
  /** Width of the "Add tag…" field. */
  inputWidth?: number;
  className?: string;
}

/**
 * An editable list of tags: each one a chip that removes it when chosen
 * (click, Enter or Space), then a field that adds what is typed on Enter,
 * comma or leaving the field. Tags are stored the way dashboards always
 * stored them: trimmed, lower case, spaces as hyphens. Backspace in the
 * empty field removes the last one.
 */
export function TagInput({ value, onChange, inputWidth = 120, className }: TagInputProps) {
  const [draft, setDraft] = useState("");
  const commit = () => {
    const tag = draft.trim().toLowerCase().replace(/\s+/g, "-");
    if (tag && !value.includes(tag)) onChange([...value, tag]);
    setDraft("");
  };
  const remove = (tag: string) => onChange(value.filter((x) => x !== tag));
  return (
    <div className={["mvx-tag-input", className].filter(Boolean).join(" ")}>
      {value.map((t) => (
        <FilterChip key={t} onClick={() => remove(t)} onClear={() => remove(t)} aria-label={`Remove tag ${t}`} title="Remove tag">
          {t}
        </FilterChip>
      ))}
      <TextInput
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === ",") {
            e.preventDefault();
            commit();
          } else if (e.key === "Backspace" && draft === "" && value.length > 0) {
            onChange(value.slice(0, -1));
          }
        }}
        onBlur={commit}
        placeholder="Add tag…"
        aria-label="Add tag"
        style={{ width: inputWidth }}
      />
    </div>
  );
}

interface TagFilterProps {
  /** Every tag in the list being filtered, in display order. */
  tags: string[];
  active: string | null;
  onChange: (tag: string | null) => void;
}

/** One chip per tag; choosing one filters the list to it, choosing it again clears the filter. */
export function TagFilter({ tags, active, onChange }: TagFilterProps) {
  return (
    <>
      {tags.map((t) => (
        <FilterChip
          key={t}
          active={active === t}
          aria-pressed={active === t}
          onClick={() => onChange(active === t ? null : t)}
          onClear={active === t ? () => onChange(null) : undefined}
        >
          {t}
        </FilterChip>
      ))}
    </>
  );
}
