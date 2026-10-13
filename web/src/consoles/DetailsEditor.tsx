import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, withTenant } from "../api/client";
import { Button, Field, TextInput, Textarea } from "../ui";

// The server's limits (internal/gateway/details_patch.go).
const MAX_NAME = 120;
const MAX_DESCRIPTION = 300;

/**
 * Rename an application or a model, or say what it is for. For the tenant or
 * platform administrator, who creates them; the description shows under the
 * name here and where business users choose an application or model.
 */
export function DetailsEditor({
  kind,
  id,
  tenantId,
  name,
  description,
  onClose,
}: {
  kind: "application" | "model";
  id: string;
  /** The tenant the row belongs to: the list spans databases (X-Tenant-Id). */
  tenantId: string;
  name: string;
  description: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [newName, setNewName] = useState(name);
  const [newDescription, setNewDescription] = useState(description);
  const save = useMutation({
    mutationFn: () => {
      const body = { name: newName.trim(), description: newDescription.trim() };
      const call = () => (kind === "application" ? api.updateAdminApplication(id, body) : api.updateAdminModel(id, body));
      return tenantId ? withTenant(tenantId, call) : call();
    },
    onSuccess: () => {
      for (const key of ["dev-applications", "admin-tenants", "apps"]) qc.invalidateQueries({ queryKey: [key] });
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
      <Field label={`${noun} name`} required>
        <TextInput value={newName} maxLength={MAX_NAME} autoFocus onChange={(e) => setNewName(e.target.value)} />
      </Field>
      <Field label="Description" description={`What it is for, shown under its name (${newDescription.length}/${MAX_DESCRIPTION}).`}>
        <Textarea value={newDescription} maxLength={MAX_DESCRIPTION} rows={2} onChange={(e) => setNewDescription(e.target.value)} />
      </Field>
      {save.isError && <p className="mvx-admin-error" role="alert">Not saved: {(save.error as Error).message}</p>}
      <div style={{ display: "flex", gap: 8 }}>
        <Button variant="primary" size="sm" disabled={!newName.trim()} loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>
          Save
        </Button>
        <Button size="sm" onClick={onClose}>Cancel</Button>
      </div>
    </div>
  );
}
