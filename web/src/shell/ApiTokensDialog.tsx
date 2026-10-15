import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button, Dialog, Field, InlineAlert, Select, TextInput, useConfirm } from "../ui";
import { api, type ApiToken } from "../api/client";
import { CopyField } from "./ChatConnectorDialog";

/**
 * Personal API tokens (Account › API tokens): a credential for a script
 * that uses the API as the person who made it — their access, nothing more.
 * A script used to borrow the console's own sign-in token, which expires
 * within the hour. Every role opens the account menu, so every person finds
 * it. The token is shown once, in the answer that made it.
 */
const EXPIRIES = [
  { days: 7, label: "7 days" },
  { days: 30, label: "30 days" },
  { days: 90, label: "90 days" },
  { days: 180, label: "180 days" },
  { days: 365, label: "1 year" },
];

function when(iso: string | null): string {
  return iso ? new Date(iso).toLocaleDateString() : "never";
}

export function ApiTokensDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [name, setName] = useState("");
  const [access, setAccess] = useState<"read" | "write">("read");
  const [days, setDays] = useState(90);
  const [made, setMade] = useState<{ token: string; name: string } | null>(null);

  const list = useQuery({ queryKey: ["api-tokens"], queryFn: api.listApiTokens, enabled: open });
  const create = useMutation({
    mutationFn: () => api.createApiToken({ name: name.trim(), access, expires_in_days: days }),
    onSuccess: (out) => {
      setMade({ token: out.token, name: out.api_token.name });
      setName("");
      void qc.invalidateQueries({ queryKey: ["api-tokens"] });
    },
  });
  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeApiToken(id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["api-tokens"] }),
  });
  const close = () => {
    setMade(null);
    create.reset();
    onClose();
  };

  return (
    <Dialog open={open} onClose={close} title="API tokens" width={600}>
      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        <p style={{ fontSize: 12, margin: 0, color: "var(--color-text-muted)" }}>
          A token lets a script use the API as you, with your access and nothing more. Send it in the
          header <code>Authorization: Bearer &lt;token&gt;</code>. A read-only token reads; a read and write token
          also loads and changes data. Revoke a token the moment you no longer need it.
        </p>

        {made && (
          <InlineAlert tone="success">
            <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              <span>Copy the token for “{made.name}” now: it is shown only once.</span>
              <CopyField label="Token" value={made.token} />
            </div>
          </InlineAlert>
        )}

        <form
          style={{ display: "grid", gridTemplateColumns: "1fr 150px 110px auto", gap: 8, alignItems: "end" }}
          onSubmit={(e) => { e.preventDefault(); if (name.trim()) create.mutate(); }}
        >
          <Field label="Name">
            <TextInput value={name} maxLength={100} placeholder="What will use it, e.g. nightly load" onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Access">
            <Select value={access} onChange={(e) => setAccess(e.target.value as "read" | "write")}>
              <option value="read">Read only</option>
              <option value="write">Read and write</option>
            </Select>
          </Field>
          <Field label="Expires in">
            <Select value={String(days)} onChange={(e) => setDays(Number(e.target.value))}>
              {EXPIRIES.map((x) => <option key={x.days} value={x.days}>{x.label}</option>)}
            </Select>
          </Field>
          <Button type="submit" variant="primary" disabled={!name.trim()} loading={create.isPending} loadingLabel="Creating…">
            Create token
          </Button>
        </form>
        {create.isError && <p className="mvx-admin-error" role="alert">{(create.error as Error).message}</p>}

        {list.isLoading && <p style={{ fontSize: 12, margin: 0 }}>Loading…</p>}
        {list.error && <p className="mvx-admin-error">{(list.error as Error).message}</p>}
        {list.data && list.data.length === 0 && <p className="mvx-admin-muted" style={{ margin: 0 }}>You have no API tokens.</p>}
        {list.data && list.data.length > 0 && (
          <table className="mvx-table mvx-table--compact" aria-label="Your API tokens">
            <thead>
              <tr><th>Name</th><th>Access</th><th>Expires</th><th>Last used</th><th /></tr>
            </thead>
            <tbody>
              {list.data.map((t: ApiToken) => (
                <tr key={t.id}>
                  <td>
                    <div>{t.name}</div>
                    <div className="mvx-admin-muted" style={{ fontSize: 11, fontFamily: "var(--font-mono, monospace)" }}>{t.hint}…</div>
                  </td>
                  <td>{t.access === "write" ? "Read and write" : "Read only"}</td>
                  <td>{t.expired ? "Expired" : when(t.expires_at)}</td>
                  <td>{when(t.last_used_at)}</td>
                  <td style={{ textAlign: "right" }}>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Revoke ${t.name}`}
                      disabled={revoke.isPending}
                      onClick={() => confirm({
                        title: "Revoke this token?",
                        body: `Scripts using “${t.name}” stop working at once. This cannot be undone.`,
                        confirmLabel: "Revoke",
                        onConfirm: () => revoke.mutate(t.id),
                      })}
                    >
                      Revoke
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {revoke.isError && <p className="mvx-admin-error" role="alert">Not revoked: {(revoke.error as Error).message}</p>}
      </div>
      {confirmElement}
    </Dialog>
  );
}
