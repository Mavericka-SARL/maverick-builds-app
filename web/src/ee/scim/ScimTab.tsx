import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type ScimToken } from "../../api/client";
import { FeatureGate } from "../../license/FeatureGate";
import { Button, Card, Field, InlineAlert, LoadingState, Select, TextInput, useConfirm } from "../../ui";

/**
 * Admin › Provisioning (SCIM): tokens the tenant's directory uses to create,
 * update, deactivate and delete users here, and to keep groups in step with
 * business roles. A token is shown once; the platform keeps only its hash.
 */
export function ScimTab() {
  return (
    <FeatureGate feature="scim">
      <ScimTokens />
    </FeatureGate>
  );
}

function ScimTokens() {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const { data, isLoading, error } = useQuery({ queryKey: ["scim-tokens"], queryFn: api.listScimTokens });
  const [name, setName] = useState("");
  const [role, setRole] = useState("business_user");
  const [issued, setIssued] = useState<ScimToken | null>(null);

  const create = useMutation({
    mutationFn: () => api.createScimToken({ name, default_role: role }),
    onSuccess: (tok) => { setIssued(tok); setName(""); void qc.invalidateQueries({ queryKey: ["scim-tokens"] }); },
  });
  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeScimToken(id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["scim-tokens"] }),
  });

  if (isLoading) return <LoadingState label="Loading provisioning tokens…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;

  const revokeToken = (t: ScimToken) =>
    confirm({
      title: `Revoke "${t.name}"?`,
      body: "Your directory stops being able to provision with it immediately. Users it created stay.",
      confirmLabel: "Revoke",
      destructive: true,
      onConfirm: () => revoke.mutate(t.id),
    });

  return (
    <div className="mvx-admin-stack" data-testid="scim-tokens">
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Connect your directory</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          Entra ID, Okta and other SCIM 2.0 directories create and deactivate your users here, and their groups become
          business roles. Point the directory at the base URL below with a token from this page as the bearer secret.
        </p>
        <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
          <Field label="Token name" description="Which directory this is for, e.g. 'Entra ID production'">
            <TextInput value={name} onChange={(e) => setName(e.target.value)} aria-label="Token name" />
          </Field>
          <Field label="Role a provisioned user gets">
            <Select value={role} onChange={(e) => setRole(e.target.value)} aria-label="Default role">
              <option value="business_user">business_user</option>
              <option value="business_admin">business_admin</option>
              <option value="developer">developer</option>
            </Select>
          </Field>
        </div>
        <div className="mvx-admin-inline-form" style={{ marginTop: 12 }}>
          <Button variant="primary" loading={create.isPending} loadingLabel="Issuing…" disabled={!name.trim()} onClick={() => create.mutate()}>
            Issue token
          </Button>
          {create.isError && <span className="mvx-admin-error">{(create.error as Error).message}</span>}
        </div>
        {issued && (
          <InlineAlert tone="success">
            <div><b>Copy this token now — it is not shown again.</b></div>
            <div style={{ marginTop: 6 }}>SCIM base URL: <code data-testid="scim-base-url">{issued.base_url}</code></div>
            <div style={{ marginTop: 4 }}>Bearer token: <code data-testid="scim-token" style={{ wordBreak: "break-all" }}>{issued.token}</code></div>
          </InlineAlert>
        )}
      </Card>

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 8 }}>Tokens</div>
        {(data ?? []).length === 0 ? (
          <p className="mvx-admin-muted">No tokens yet.</p>
        ) : (
          <table className="mvx-table">
            <thead><tr><th>Name</th><th>Role</th><th>Created</th><th>Last used</th><th>Status</th><th /></tr></thead>
            <tbody>
              {(data ?? []).map((t) => (
                <tr key={t.id} data-testid={`scim-token-${t.id}`}>
                  <td style={{ fontWeight: 600 }}>{t.name}</td>
                  <td>{t.default_role}</td>
                  <td className="mvx-admin-muted">{t.created_at.slice(0, 10)}</td>
                  <td className="mvx-admin-muted">{t.last_used_at ? t.last_used_at.slice(0, 16).replace("T", " ") : "never"}</td>
                  <td>{t.revoked_at ? "Revoked" : "Active"}</td>
                  <td>{!t.revoked_at && <Button size="sm" variant="dangerSecondary" onClick={() => revokeToken(t)}>Revoke</Button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
      {confirmElement}
    </div>
  );
}
