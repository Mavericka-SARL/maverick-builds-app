import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import { Card, Checkbox, InlineAlert, LoadingState } from "../../ui";

/**
 * Admin › Chat connector: whether this tenant's people may change data from
 * ChatGPT or Claude. A connection writes only the input cells its person may
 * write, and changes only the records they may change, in the console, and
 * only once they allowed it to change data when connecting; this switch stops
 * every such change for the whole tenant.
 * Reads through a connection, and the console itself, are not affected.
 */
export function ChatConnectorSettingsTab() {
  const qc = useQueryClient();
  const { data, isLoading, error } = useQuery({ queryKey: ["connector-settings"], queryFn: api.getConnectorSettings });
  // The box shows the new state at once while it saves, and the saved one
  // after — the old one again if the save fails.
  const [pending, setPending] = useState<boolean | null>(null);
  const save = useMutation({
    mutationFn: (chatWrites: boolean) => api.updateConnectorSettings({ chat_writes: chatWrites }),
    onSuccess: (next) => qc.setQueryData(["connector-settings"], next),
    onSettled: () => setPending(null),
  });

  if (isLoading) return <LoadingState label="Loading chat connector settings…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;

  return (
    <div className="mvx-admin-stack" data-testid="connector-settings">
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Changes from ChatGPT and Claude</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          People who connect ChatGPT or Claude to their account can ask it to enter values into the grids they can edit
          here — numbers, texts, dates and choices, in the active revision — and to update the records they can change,
          such as a deal's stage or amount. Each change is made as that person, under their own access rules, workflow
          locks and record permissions, and is kept in the cell or record history and the audit log. Turn this off to
          let connections read only.
        </p>
        <Checkbox
          checked={pending ?? data?.chat_writes ?? true}
          disabled={save.isPending}
          onChange={(e) => {
            setPending(e.target.checked);
            save.mutate(e.target.checked);
          }}
          label="Allow changing grid values and records from a chat"
        />
        {save.error && <InlineAlert tone="danger">{(save.error as Error).message}</InlineAlert>}
      </Card>
    </div>
  );
}
