import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, Trash2 } from "lucide-react";
import { api, type DimInfo, type DimMember } from "../../api/client";
import { Button, IconButton, TextInput, useConfirm } from "../../ui";
import { invalidateModelData } from "../modelDataQueries";

// A business-maintained dimension's rows (the developer marks the dimension;
// business users keep its members): a row's label is renamed or the row
// removed in place, and new rows are added under the table. The server holds
// each change to the user's access and the open revision.

function useMemberChange() {
  const qc = useQueryClient();
  return () => invalidateModelData(qc);
}

// MemberLabel is a row header's label with rename and remove, for a leaf
// member of a business-maintained dimension.
export function MemberLabel({ dim, member }: { dim: DimInfo; member: DimMember }) {
  const refresh = useMemberChange();
  const [editing, setEditing] = useState(false);
  const [label, setLabel] = useState(member.label);
  const { confirm, confirmElement } = useConfirm();
  const rename = useMutation({
    mutationFn: () => api.updateBusinessMember(dim.id, member.id, { label: label.trim() }),
    onSuccess: async () => { await refresh(); setEditing(false); },
  });
  const remove = useMutation({
    mutationFn: () => api.deleteBusinessMember(dim.id, member.id),
    onSuccess: () => refresh(),
  });
  if (editing) {
    return (
      <span style={{ display: "inline-flex", gap: 4, alignItems: "center" }}>
        <TextInput value={label} autoFocus aria-label={`Rename ${member.label}`} style={{ width: 180 }}
          onChange={e => setLabel(e.target.value)}
          onKeyDown={e => {
            if (e.key === "Enter" && label.trim()) rename.mutate();
            if (e.key === "Escape") { setEditing(false); setLabel(member.label); }
          }} />
        <Button size="sm" variant="primary" disabled={!label.trim()} loading={rename.isPending} onClick={() => rename.mutate()}>Save</Button>
        {rename.isError && <span className="mvx-admin-error">{(rename.error as Error).message}</span>}
      </span>
    );
  }
  return (
    <span className="mvx-member-label" style={{ display: "inline-flex", gap: 4, alignItems: "center" }}>
      {member.label}
      {!member.readonly && (
        <span className="mvx-member-label__actions">
          <IconButton aria-label={`Rename ${member.label}`} size={20} onClick={e => { e.stopPropagation(); setEditing(true); }}>
            <Pencil size={11} />
          </IconButton>
          <IconButton aria-label={`Remove ${member.label}`} size={20} onClick={e => {
            e.stopPropagation();
            confirm({
              title: `Remove ${member.label}?`,
              body: `The row and the values entered on it are removed (they stay in the cell history).`,
              confirmLabel: "Remove", onConfirm: () => remove.mutate(),
            });
          }}>
            <Trash2 size={11} />
          </IconButton>
        </span>
      )}
      {remove.isError && <span className="mvx-admin-error">{(remove.error as Error).message}</span>}
      {confirmElement}
    </span>
  );
}

// AddMemberRow adds a member to a business-maintained dimension: its name,
// and its parent when the dimension's leaves sit under parents.
export function AddMemberRow({ dim, colSpan }: { dim: DimInfo; colSpan: number }) {
  const refresh = useMemberChange();
  const parents = dim.members.filter(m => dim.members.some(c => c.parent_code === m.code) && !m.formula);
  const [open, setOpen] = useState(false);
  const [label, setLabel] = useState("");
  const [parentId, setParentId] = useState(parents[0]?.id ?? "");
  const add = useMutation({
    mutationFn: () => api.addBusinessMember(dim.id, { label: label.trim(), ...(parentId ? { parent_member_id: parentId } : {}) }),
    onSuccess: async () => { await refresh(); setLabel(""); setOpen(false); },
  });
  return (
    <tr>
      <td colSpan={colSpan} style={{ padding: "6px 12px" }}>
        {open ? (
          <span style={{ display: "inline-flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
            <TextInput value={label} autoFocus placeholder={`New ${dim.name} name`} aria-label={`New ${dim.name} name`} style={{ width: 220 }}
              onChange={e => setLabel(e.target.value)}
              onKeyDown={e => { if (e.key === "Enter" && label.trim()) add.mutate(); if (e.key === "Escape") setOpen(false); }} />
            {parents.length > 1 && (
              <select aria-label="Under" value={parentId} onChange={e => setParentId(e.target.value)} className="mvx-cell-input mvx-cell-select">
                {parents.map(p => <option key={p.id} value={p.id}>under {p.label}</option>)}
              </select>
            )}
            <Button size="sm" variant="primary" disabled={!label.trim()} loading={add.isPending} onClick={() => add.mutate()}>Add</Button>
            <Button size="sm" onClick={() => setOpen(false)}>Cancel</Button>
            {add.isError && <span className="mvx-admin-error">{(add.error as Error).message}</span>}
          </span>
        ) : (
          <Button size="sm" variant="ghost" onClick={() => setOpen(true)}>
            <Plus size={13} /> Add {dim.name.replace(/_/g, " ")}
          </Button>
        )}
      </td>
    </tr>
  );
}
