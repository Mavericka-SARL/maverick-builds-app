import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, withTenant } from "../api/client";
import { Button, Field, Select, TextInput, Textarea } from "../ui";
import { DATE_FORMATS, formatIsoDate } from "./dateSerial";

// The server's limits (internal/gateway/details_patch.go).
const MAX_NAME = 120;
const MAX_DESCRIPTION = 300;

/**
 * Rename an application or a model, or say what it is for. For the tenant or
 * platform administrator, who creates them; the description shows under the
 * name here and where business users choose an application or model.
 *
 * A model's date format is edited here too, beside its description (owner,
 * 2026-11-18), by the developer, who sets it (PUT
 * /api/developer/models/{id}/date-format): dateFormat given shows it.
 * canRename false leaves name and description to the administrator — a
 * developer who is not one edits the date format alone.
 */
export function DetailsEditor({
  kind,
  id,
  tenantId,
  name,
  description,
  dateFormat,
  canRename = true,
  onClose,
}: {
  kind: "application" | "model";
  id: string;
  /** The tenant the row belongs to: the list spans databases (X-Tenant-Id). */
  tenantId: string;
  name: string;
  description: string;
  /** A model's date format ("" each browser's); undefined: not edited here. */
  dateFormat?: string;
  canRename?: boolean;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [newName, setNewName] = useState(name);
  const [newDescription, setNewDescription] = useState(description);
  const [newDateFormat, setNewDateFormat] = useState(dateFormat ?? "");
  const save = useMutation({
    mutationFn: async () => {
      const inTenant = <T,>(call: () => Promise<T>) => (tenantId ? withTenant(tenantId, call) : call());
      const body = { name: newName.trim(), description: newDescription.trim() };
      if (canRename && (body.name !== name || body.description !== description)) {
        await inTenant(() => (kind === "application" ? api.updateAdminApplication(id, body) : api.updateAdminModel(id, body)));
      }
      if (dateFormat !== undefined && newDateFormat !== dateFormat) {
        await inTenant(() => api.setModelDateFormat(id, newDateFormat));
      }
    },
    onSuccess: () => {
      // The date format reads through /api/demo on every business page.
      for (const key of ["dev-applications", "admin-tenants", "apps", "demo"]) qc.invalidateQueries({ queryKey: [key] });
      onClose();
    },
  });
  const noun = kind === "application" ? "Application" : "Model";

  return (
    <div
      className="mvx-panel"
      style={{ display: "flex", flexDirection: "column", gap: 8, padding: 12, margin: "8px 0" }}
      aria-label={`Edit ${kind} ${name}`}
      role="group"
    >
      {canRename && (<>
        <Field label={`${noun} name`} required>
          <TextInput value={newName} maxLength={MAX_NAME} autoFocus onChange={(e) => setNewName(e.target.value)} />
        </Field>
        <Field label="Description" description={`What it is for, shown under its name (${newDescription.length}/${MAX_DESCRIPTION}).`}>
          <Textarea value={newDescription} maxLength={MAX_DESCRIPTION} rows={2} onChange={(e) => setNewDescription(e.target.value)} />
        </Field>
      </>)}
      {dateFormat !== undefined && (
        <Field label="Date format" description="How dates read for everyone using this model: grids, tables of records, KPIs and date fields.">
          <Select value={newDateFormat} onChange={(e) => setNewDateFormat(e.target.value)} style={{ maxWidth: 320 }}>
            <option value="">Each browser's format</option>
            {DATE_FORMATS.map((f) => <option key={f} value={f}>{f} ({formatIsoDate("2026-12-31", f)})</option>)}
          </Select>
        </Field>
      )}
      {save.isError && <p className="mvx-admin-error" role="alert">Not saved: {(save.error as Error).message}</p>}
      <div style={{ display: "flex", gap: 8 }}>
        <Button variant="primary" size="sm" disabled={canRename && !newName.trim()} loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>
          Save
        </Button>
        <Button size="sm" onClick={onClose}>Cancel</Button>
      </div>
    </div>
  );
}
