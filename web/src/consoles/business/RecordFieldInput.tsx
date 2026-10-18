import { Plus as PlusIcon } from "lucide-react";
import { type Metric, type DevDimension, type DevDimensionMember, type FormField } from "../../api/client";
import { Button, Select, TextInput, NumberInput, StatusBadge, Textarea } from "../../ui";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { statusLabel } from "./recordList";

// A record's dimension field only ever lets the user pick a real, concrete
// member (e.g. a specific department) — never an aggregate/rollup node,
// unlike the chart/grid context selectors' HierarchicalMemberSelect usage
// (see HierarchicalMemberSelect's leafOnly prop). DevDimensionMember's
// parent_member_id is an ID reference (developer-console shape), not the
// parent_code the shared tree builder keys on, so each member is adapted
// to carry its resolved parent's code instead.
interface DimFormMember extends DevDimensionMember {
  parent_code?: string;
}

function toDimFormMembers(allMembers: DevDimensionMember[]): DimFormMember[] {
  const byId = new Map(allMembers.map(m => [m.id, m]));
  return allMembers.map(m => ({
    ...m,
    label: m.label || m.code,
    parent_code: m.parent_member_id ? byId.get(m.parent_member_id)?.code : undefined,
  }));
}

// ── Shared record field input renderer ────────────────────────────────────────

// id is the one Field clones onto its child, so the field's label names the
// control it renders. readOnlyMembersOut: the record is the person's own and
// posts this field's member into a metric — within its creator's access —
// so members the person may only read are not offered (the server refuses
// such a save). onCreateLinked: the field names a member a table keeps
// (an account), and the person may add a record to it — "+ New" beside the
// picker opens that table's form.
export function FormFieldInput({
  f, value, onChange, dims, metrics, id, readOnlyMembersOut, onCreateLinked, linkedLabel,
}: {
  f: FormField;
  value: unknown;
  onChange: (v: unknown) => void;
  dims: DevDimension[];
  metrics: Metric[];
  id?: string;
  readOnlyMembersOut?: boolean;
  onCreateLinked?: () => void;
  linkedLabel?: string;
}) {
  if (f.type === "autocode") {
    // The server gives the code; nobody types it.
    return <TextInput id={id} value={value ? String(value) : "Given when the record is saved"} readOnly disabled style={{ width: "100%" }} />;
  }
  if (f.type === "dimension") {
    const dim = dims.find(d => d.id === f.dimension_id) ?? dims.find(d => d.name === f.dimension_id);
    const allMembers = toDimFormMembers(dim?.members ?? []);
    const allowedSet = f.allowed_members && f.allowed_members.length > 0 ? new Set(f.allowed_members) : null;
    const selectable = allowedSet || readOnlyMembersOut
      ? (m: DimFormMember) => (!allowedSet || allowedSet.has(m.code)) && !(readOnlyMembersOut && m.access === "read")
      : undefined;
    const picker = (
      <HierarchicalMemberSelect
        id={id}
        ariaLabel={f.label || dim?.name || "Select…"}
        members={allMembers}
        value={String(value ?? "")}
        onChange={onChange}
        placeholder="Select…"
        leafOnly
        isSelectable={selectable}
        className="mvx-hier-select__trigger--full-width"
      />
    );
    if (!onCreateLinked) return picker;
    return (
      <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
        <div style={{ flex: 1, minWidth: 0 }}>{picker}</div>
        <Button size="sm" variant="ghost" leadingIcon={<PlusIcon size={13} />} onClick={onCreateLinked}
          aria-label={`New ${linkedLabel ?? f.label}`}>
          New
        </Button>
      </div>
    );
  }
  if (f.type === "metric") {
    if (f.metric_id) {
      // Pre-configured metric — just enter the amount.
      return (
        <NumberInput
          id={id}
          value={String(value ?? "")}
          onChange={e => onChange(e.target.value === "" ? "" : parseFloat(e.target.value))}
          style={{ width: "100%" }}
        />
      );
    }
    // Dynamic metric selection — choose which input metric to write to.
    return (
      <Select id={id} value={String(value ?? "")} onChange={e => onChange(e.target.value)} style={{ width: "100%" }}>
        <option value="">Select metric…</option>
        {metrics.filter(m => m.is_input).map(m => <option key={m.id} value={m.name}>{m.label || m.name}</option>)}
      </Select>
    );
  }
  if (f.type === "select") {
    return (
      <Select id={id} value={String(value ?? "")} onChange={e => onChange(e.target.value)} style={{ width: "100%" }}>
        <option value="">Select…</option>
        {(f.options ?? []).map(o => <option key={o} value={o}>{o}</option>)}
      </Select>
    );
  }
  if (f.type === "boolean") {
    return (
      <input type="checkbox" className="mvx-checkbox" id={id}
        checked={!!value}
        onChange={e => onChange(e.target.checked)}
      />
    );
  }
  if (f.type === "longtext") {
    return (
      <Textarea id={id} rows={3} value={String(value ?? "")} onChange={e => onChange(e.target.value)} style={{ width: "100%" }} />
    );
  }
  return (
    <TextInput
      id={id}
      type={f.type === "number" ? "number" : f.type === "date" ? "date" : f.type === "url" ? "url" : "text"}
      step={f.type === "number" && f.format === "money" ? "0.01" : undefined}
      placeholder={f.type === "url" ? "https://…" : undefined}
      value={String(value ?? "")}
      onChange={e => onChange(f.type === "number" ? (e.target.value === "" ? "" : parseFloat(e.target.value)) : e.target.value)}
      style={{ width: "100%" }}
    />
  );
}

/** A record's status, read-only; a record without one says so. */
export function RecordStatusBadge({ status }: { status: string }) {
  return (
    <StatusBadge tone={status === "approved" ? "success" : status === "rejected" ? "danger" : status === "submitted" ? "warning" : "neutral"}>
      {statusLabel(status)}
    </StatusBadge>
  );
}
