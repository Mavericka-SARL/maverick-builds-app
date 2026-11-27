import type { FormDef, GanttSettings } from "../../api/client";
import { Field, Select } from "../../ui";

/**
 * A Gantt widget's fields (widget_props.gantt, GanttPanel.tsx): the date
 * fields its bars and baselines run between, and the fields that name,
 * group, colour, fill and link them.
 */
export function GanttSettingsEditor({ form, value, onChange }: {
  form: FormDef;
  value: GanttSettings;
  onChange: (v: GanttSettings) => void;
}) {
  const dates = form.fields.filter(f => f.type === "date");
  const numbers = form.fields.filter(f => f.type === "number" || f.type === "metric");
  const any = form.fields.filter(f => f.type !== "status");
  const set = (k: keyof GanttSettings, v: string) => onChange({ ...value, [k]: v || undefined } as GanttSettings);
  const pick = (k: keyof GanttSettings, label: string, fields: typeof any, description?: string, required?: boolean) => (
    <Field label={label} description={description}>
      <Select value={(value[k] as string | undefined) ?? ""} onChange={e => set(k, e.target.value)} aria-label={label}>
        <option value="">{required ? "— select —" : "— none —"}</option>
        {k === "color_field" && form.fields.some(f => f.type === "status") && <option value="status">Status</option>}
        {fields.map(f => <option key={f.name} value={f.name}>{f.label || f.name}</option>)}
      </Select>
    </Field>
  );
  if (dates.length === 0) {
    return <p className="mvx-admin-muted" style={{ fontSize: 12 }}>{form.label || form.name} has no date field: a timeline needs a start date.</p>;
  }
  return (
    <>
      {pick("start_field", "Start date", dates, "Each record's bar starts here.", true)}
      {pick("end_field", "End date", dates, "None, or the start day: a milestone.")}
      {pick("label_field", "Row label", any, "The table's label field when none.")}
      {pick("baseline_start_field", "Baseline start", dates, "Drawn under the bar: planned against current dates.")}
      {pick("baseline_end_field", "Baseline end", dates)}
      {pick("group_field", "Group rows by", any, "A project, a work package.")}
      {pick("color_field", "Colour bars by", any, "A status, a priority.")}
      {pick("progress_field", "Progress (%)", numbers, "Fills the part done.")}
      {pick("predecessor_field", "Follows", any, "A field naming the record this one follows: an arrow, red when it starts before that one ends.")}
    </>
  );
}
