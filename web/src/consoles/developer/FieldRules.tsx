import { Plus, Trash2 } from "lucide-react";
import type { FormField, FieldCondition, FieldTransition } from "../../api/client";
import { TextInput, Select, Checkbox, IconButton, Button } from "../../ui";
import { RolePicker } from "./RolePicker";

/** Values typed as a list: "New, Open". */
const splitValues = (s: string) => s.split(",").map(v => v.trim()).filter(Boolean);
const joinValues = (vs?: string[]) => (vs ?? []).join(", ");

const row = { display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap" as const, fontSize: 12 };

/** A condition on another field: it is (or is not) one of some values. */
function ConditionEditor({ label, value, others, onChange }: {
  label: string;
  value?: FieldCondition;
  others: FormField[];
  onChange: (c?: FieldCondition) => void;
}) {
  const negated = !!value?.is_not?.length;
  const values = negated ? value?.is_not : value?.is;
  const set = (field: string, not: boolean, vs: string[]) =>
    onChange(field ? (not ? { field, is_not: vs } : { field, is: vs }) : undefined);
  const other = others.find(o => o.name === value?.field);
  return (
    <div style={row}>
      <span className="mvx-admin-muted">{label}</span>
      <Select style={{ width: 200 }} value={value?.field ?? ""} aria-label={`${label} field`}
        onChange={e => set(e.target.value, negated, values ?? [])}>
        <option value="">— never —</option>
        {others.map(o => <option key={o.name} value={o.name}>{o.label || o.name}</option>)}
      </Select>
      {value?.field && (
        <>
          <Select style={{ width: 110 }} value={negated ? "not" : "is"} aria-label={`${label} is or is not`}
            onChange={e => set(value.field, e.target.value === "not", values ?? [])}>
            <option value="is">is one of</option>
            <option value="not">is none of</option>
          </Select>
          <TextInput value={joinValues(values)} aria-label={`${label} values`} style={{ width: 200 }}
            placeholder={other?.options?.slice(0, 2).join(", ") || "values or member codes"}
            onChange={e => set(value.field, negated, splitValues(e.target.value))} />
        </>
      )}
    </div>
  );
}

/** A select or dimension field's moves: each row from some values to others, optionally kept for some roles. */
function TransitionsEditor({ f, applicationId, onUpdate }: {
  f: FormField;
  applicationId?: string;
  onUpdate: (patch: Partial<FormField>) => void;
}) {
  const list = f.transitions ?? [];
  const setAt = (i: number, patch: Partial<FieldTransition>) =>
    onUpdate({ transitions: list.map((t, idx) => idx === i ? { ...t, ...patch } : t) });
  const hint = f.type === "select" ? (f.options ?? []).join(", ") : "member codes";
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div style={row}>
        <span className="mvx-admin-muted">
          Moves once set ({hint}). None: any value may follow any. A save making another move is refused, by every route.
        </span>
      </div>
      {list.map((t, i) => (
        <div key={i} style={{ ...row, paddingLeft: 12 }} role="group" aria-label={`Move ${i + 1}`}>
          <span className="mvx-admin-muted">from</span>
          <TextInput value={joinValues(t.from)} aria-label={`Move ${i + 1} from`} style={{ width: 160 }} placeholder="New"
            onChange={e => setAt(i, { from: splitValues(e.target.value) })} />
          <span className="mvx-admin-muted">to</span>
          <TextInput value={joinValues(t.to)} aria-label={`Move ${i + 1} to`} style={{ width: 200 }} placeholder="Open, Resolved"
            onChange={e => setAt(i, { to: splitValues(e.target.value) })} />
          <IconButton aria-label={`Remove move ${i + 1}`} title="Remove move" danger size={24}
            onClick={() => onUpdate({ transitions: list.filter((_, idx) => idx !== i).length ? list.filter((_, idx) => idx !== i) : undefined })}>
            <Trash2 size={12} />
          </IconButton>
          {applicationId && (
            <div style={{ flexBasis: "100%", paddingLeft: 12 }}>
              <span className="mvx-admin-muted">Only by (none ticked: anyone who may change the record; administrators always may):</span>
              <RolePicker applicationId={applicationId} value={t.roles ?? []} onChange={roles => setAt(i, { roles: roles.length ? roles : undefined })} />
            </div>
          )}
        </div>
      ))}
      <div style={{ paddingLeft: 12 }}>
        <Button size="sm" variant="ghost" leadingIcon={<Plus size={12} />}
          onClick={() => onUpdate({ transitions: [...list, { from: [], to: [] }] })}>
          Add move
        </Button>
      </div>
      <div style={row}>
        <span className="mvx-admin-muted">Read-only while it is one of</span>
        <TextInput value={joinValues(f.locks_in)} aria-label="Read-only while it is" style={{ width: 200 }} placeholder="Closed"
          onChange={e => { const v = splitValues(e.target.value); onUpdate({ locks_in: v.length ? v : undefined }); }} />
        <span className="mvx-admin-muted">— only this field may then change (a closed ticket is reopened, not edited)</span>
      </div>
    </div>
  );
}

/**
 * A field's opt-in rules (help desk, 2026-11-20): moves and locks on a
 * select or dimension field, required in some states, a date the server
 * stamps, a boolean marking a member active, and a person field's roles and
 * notification. A field using none saves as before.
 */
export function FieldRules({ f, fields, applicationId, onUpdate }: {
  f: FormField;
  fields: FormField[];
  applicationId?: string;
  onUpdate: (patch: Partial<FormField>) => void;
}) {
  const others = fields.filter(o => o !== f && o.name && o.type !== "autocode" && o.type !== "status");
  const serverFilled = f.type === "autocode" || f.type === "status";
  return (
    <>
      {(f.type === "select" || f.type === "dimension") && !f.multiple && (
        <TransitionsEditor f={f} applicationId={applicationId} onUpdate={onUpdate} />
      )}
      {!serverFilled && !f.stamp_when && (
        <ConditionEditor label="Required when" value={f.required_when} others={others}
          onChange={c => onUpdate({ required_when: c })} />
      )}
      {f.type === "date" && (
        <ConditionEditor label="Set by the server to the day it comes to meet" value={f.stamp_when} others={others}
          onChange={c => onUpdate({ stamp_when: c, ...(c ? { default: undefined, required: false, required_with: undefined, required_when: undefined } : {}) })} />
      )}
      {f.type === "dimension" && !f.multiple && (
        <div style={row}>
          <Checkbox checked={!!f.notify} onChange={e => onUpdate({ notify: e.target.checked || undefined })}
            label="Tell the person the chosen record names (its Person field) — a ticket's assignee is an agent record" />
        </div>
      )}
      {f.type === "boolean" && (
        <div style={row}>
          <Checkbox checked={!!f.active_flag} onChange={e => onUpdate({ active_flag: e.target.checked || undefined })}
            label="Marks the record's member active — unticked, it stays where it is named and is offered for no new choice (a table keeping a dimension)" />
        </div>
      )}
      {f.type === "person" && (
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          <span className="mvx-admin-muted" style={{ fontSize: 12 }}>
            Names a person who uses the application; the picker shows names only. None ticked: anyone who opens the application.
          </span>
          {applicationId && <RolePicker applicationId={applicationId} value={f.roles ?? []} onChange={roles => onUpdate({ roles: roles.length ? roles : undefined })} />}
          <div style={row}>
            <Checkbox checked={!!f.notify} onChange={e => onUpdate({ notify: e.target.checked || undefined })}
              label="Tell the person named, in their notifications" />
          </div>
        </div>
      )}
    </>
  );
}
