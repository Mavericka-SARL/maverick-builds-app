import { Button, Dialog, Field, TextInput } from "../ui";
import type { RunParameter } from "./runParameters";

// RunParametersDialog asks for the values a loader takes at each run
// ("Period end") before its file is chosen: a dashboard's Integration
// button and the developer's Run with a file both ask them here.
export function RunParametersDialog({ open, title, params, values, onChange, onCancel, onChoose }: {
  open: boolean;
  title: string;
  params: RunParameter[];
  values: Record<string, string>;
  onChange: (values: Record<string, string>) => void;
  onCancel: () => void;
  onChoose: () => void;
}) {
  const missing = params.some(p => p.required && !(values[p.column] ?? "").trim());
  return (
    <Dialog open={open} onClose={onCancel} title={title} width={420}>
      <div style={{ display: "grid", gap: 12 }}>
        {params.map(p => (
          <Field key={p.column} label={`${p.label || p.column}${p.required ? "" : " (optional)"}`}>
            <TextInput type={p.type === "date" ? "date" : "text"} aria-label={p.label || p.column}
              value={values[p.column] ?? ""}
              onChange={e => onChange({ ...values, [p.column]: e.target.value })} />
          </Field>
        ))}
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <Button variant="ghost" onClick={onCancel}>Cancel</Button>
          <Button disabled={missing} onClick={onChoose}>Choose file…</Button>
        </div>
      </div>
    </Dialog>
  );
}
