import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import { ChevronDown, ChevronRight } from "lucide-react";
import { Button, Card, Field, InlineAlert } from "../../ui";
import { useConfirm } from "../../ui/useConfirm";

/**
 * Integrations › Google Sheets › the tenant's Google service account, which
 * makes PRIVATE Google Sheets importable: the developer pastes the key file
 * Google Cloud downloaded, the sheet's owner shares the sheet with the
 * account's address, and every Sheets import of this tenant (wizard and
 * saved integrations) reads through it. Without one, only link-shared
 * sheets work, as before. The private key never comes back from the
 * server; only the address does, because that is what people need to copy.
 * Collapsed until opened: most imports never need it.
 */
export function GoogleServiceAccountPanel() {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [open, setOpen] = useState(false);
  const { data, isLoading, error } = useQuery({ queryKey: ["google-service-account"], queryFn: api.getGoogleServiceAccount });
  const [keyFile, setKeyFile] = useState("");
  const onSaved = (next: Awaited<ReturnType<typeof api.getGoogleServiceAccount>>) => {
    qc.setQueryData(["google-service-account"], next);
    setKeyFile("");
  };
  const save = useMutation({ mutationFn: () => api.putGoogleServiceAccount(keyFile), onSuccess: onSaved });
  const remove = useMutation({ mutationFn: api.deleteGoogleServiceAccount, onSuccess: onSaved });
  const test = useMutation({ mutationFn: api.testGoogleServiceAccount });

  const summary = isLoading ? "…" : data?.configured ? `share sheets with ${data.client_email}` : "not set up — link-shared sheets only";

  return (
    <div className="mvx-panel" style={{ marginTop: 24, padding: 16 }} data-testid="google-service-account">
      {confirmElement}
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        style={{ display: "flex", alignItems: "center", gap: 8, width: "100%", background: "none", border: 0, padding: 0, cursor: "pointer", color: "var(--color-text)", textAlign: "left" }}
      >
        {open ? <ChevronDown size={16} /> : <ChevronRight size={16} />}
        <span style={{ fontWeight: 700 }}>Private sheets: Google service account</span>
        <span className="mvx-admin-muted" style={{ fontSize: 13 }} data-testid="google-service-account-summary">— {summary}</span>
      </button>
      {open && error && <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>}
      {open && !error && (
      <Card style={{ marginTop: 12 }}>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          Lets imports read <strong>private</strong> Google Sheets. In Google Cloud, create a service account (IAM › Service
          accounts), add a JSON key, and paste the downloaded file here. Then share each sheet with the account&apos;s address,
          as with any collaborator. Link-shared sheets keep working without this.
        </p>
        {data?.configured ? (
          <div style={{ display: "grid", gap: 8, marginBottom: 12 }}>
            <div>
              Share sheets with: <code data-testid="google-sa-email">{data.client_email}</code>
              {data.project_id && <span className="mvx-admin-muted"> · project {data.project_id}</span>}
            </div>
            <div style={{ display: "flex", gap: 12, alignItems: "center", flexWrap: "wrap" }}>
              <Button variant="secondary" onClick={() => test.mutate()} loading={test.isPending} loadingLabel="Testing…">
                Test the account
              </Button>
              <Button
                variant="ghost"
                onClick={() =>
                  confirm({
                    title: "Remove the Google service account?",
                    body: "Imports of private sheets will fail until another account is stored. Link-shared sheets are unaffected.",
                    confirmLabel: "Remove",
                    destructive: true,
                    onConfirm: () => remove.mutate(),
                  })
                }
                loading={remove.isPending}
                loadingLabel="Removing…"
              >
                Remove
              </Button>
              {test.isSuccess && <InlineAlert tone="success">Google issued a token for {test.data.client_email} — the key works.</InlineAlert>}
              {test.isError && <InlineAlert tone="danger">{(test.error as Error).message}</InlineAlert>}
              {remove.isError && <InlineAlert tone="danger">{(remove.error as Error).message}</InlineAlert>}
            </div>
          </div>
        ) : (
          <InlineAlert tone="info">No account stored: only link-shared sheets can be imported.</InlineAlert>
        )}
        <Field label={data?.configured ? "Replace the key file" : "Key file (JSON)"} description="Only the account's address and private key are kept, encrypted. The file itself is not stored.">
          <textarea
            value={keyFile}
            onChange={(e) => setKeyFile(e.target.value)}
            rows={6}
            placeholder='{ "type": "service_account", "client_email": "…", "private_key": "…" }'
            aria-label="Google service account key file"
            style={{ width: "100%", fontFamily: "var(--font-mono)", fontSize: 12 }}
          />
        </Field>
        <div className="mvx-admin-inline-form">
          <Button variant="primary" onClick={() => save.mutate()} disabled={!keyFile.trim()} loading={save.isPending} loadingLabel="Saving…">
            Save key
          </Button>
          {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
        </div>
      </Card>
      )}
    </div>
  );
}
