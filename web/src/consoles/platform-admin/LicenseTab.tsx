import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Check, Lock } from "lucide-react";
import { api } from "../../api/client";
import { Button, Card, Field, InlineAlert, StatusBadge, Textarea, Txt, useConfirm } from "../../ui";
import { EDITION_LABELS, licenseDate as fmtDate, useLicense } from "../../license/useLicense";

const STATE_TONE = { community: "neutral", active: "success", expired: "warning", invalid: "danger" } as const;
const STATE_LABEL = { community: "No license key", active: "Active", expired: "Expired", invalid: "Invalid" } as const;


const SOURCE_LABEL = { env: "MAVERICKS_LICENSE_KEY (server configuration)", file: "MAVERICKS_LICENSE_FILE (server configuration)", console: "Installed on this page", none: "none installed" } as const;

/**
 * Platform › License: which edition this deployment runs, why, and what each
 * other edition would add — and where a platform administrator installs a
 * Commercial or Enterprise key (owner, 2026-11-22): pasted here, verified
 * offline by the gateway and in force at once, on every replica within a
 * minute. A key in the server's configuration wins; then this page says
 * where it is set instead of offering the field.
 */
export function LicenseTab() {
  const lic = useLicense();
  const rows: Array<[string, string]> = [
    ["Edition", `${EDITION_LABELS[lic.edition]}`],
    ["Key status", STATE_LABEL[lic.state]],
    ["Customer", lic.customer || "—"],
    ["Contact", lic.contact || "—"],
    ["License id", lic.license_id || "—"],
    ["Order", lic.order || "—"],
    ["Agreement", lic.agreement || "—"],
    ["Deployment", lic.deployment || "—"],
    ["Feature schedule", lic.schedule || "—"],
    ["Issued", fmtDate(lic.issued_at)],
    ["Expires", fmtDate(lic.expires_at)],
    ["Key source", SOURCE_LABEL[lic.source] ?? lic.source],
  ];

  return (
    <div className="mvx-admin-stack" data-testid="license-tab">
      <Card>
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 12 }}>
          <span style={{ fontSize: 17, fontWeight: 700 }}>{EDITION_LABELS[lic.edition]} Edition</span>
          <StatusBadge tone={STATE_TONE[lic.state]}>{STATE_LABEL[lic.state]}</StatusBadge>
        </div>
        {lic.state === "invalid" && (
          <InlineAlert tone="danger">The configured license key could not be verified: <Txt>{lic.error}</Txt><Txt>. The deployment runs the Community Edition until a valid key is installed.</Txt></InlineAlert>
        )}
        {lic.state === "active" && lic.renewal_due && (
          <InlineAlert tone="warning">The license key expires on {fmtDate(lic.expires_at)}. Install the renewed key before then: from the day after, the deployment and every tenant run the Community Edition.</InlineAlert>
        )}
        {lic.state === "expired" && (
          <InlineAlert tone="warning">The license key expired on {fmtDate(lic.expires_at)}. The deployment and every tenant run the Community Edition until a new key is installed; nothing the paid features stored was deleted.</InlineAlert>
        )}
        <table className="mvx-table" style={{ marginTop: 8 }}>
          <tbody>
            {rows.map(([k, v]) => (
              <tr key={k}>
                <td style={{ width: 160, fontWeight: 600 }}>{k}</td>
                <td>{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 8 }}>Features by edition</div>
        {lic.catalog.length === 0 ? (
          <p className="mvx-admin-muted">The gateway did not report a feature catalog.</p>
        ) : (
          <table className="mvx-table">
            <thead>
              <tr><th>Feature</th><th>What it adds</th><th style={{ width: 120 }}>Edition</th><th style={{ width: 130 }}>Status</th></tr>
            </thead>
            <tbody>
              {lic.catalog.map((f) => {
                const on = lic.features.includes(f.key);
                return (
                  <tr key={f.key} data-testid={`feature-${f.key}`}>
                    <td style={{ fontWeight: 600 }}>{f.name}</td>
                    <td className="mvx-admin-muted">{f.description}</td>
                    <td>{EDITION_LABELS[f.min_edition]}</td>
                    <td>
                      {on
                        ? <StatusBadge tone="success"><Check size={12} aria-hidden="true" /> Included</StatusBadge>
                        : <StatusBadge tone="neutral"><Lock size={12} aria-hidden="true" /> Locked</StatusBadge>}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </Card>

      <InstallKey source={lic.source} />

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 8 }}>About license keys</div>
        <p className="mvx-admin-muted" style={{ margin: "0 0 8px" }}>
          A key is issued per deployment for an accepted order and verified offline: nothing is sent anywhere. Install it above,
          or set it in the server&apos;s configuration as <code>MAVERICKS_LICENSE_KEY</code> (the key itself) or
          <code> MAVERICKS_LICENSE_FILE</code> (a path to a file holding it) and restart the gateway; a key set there wins over one
          installed here.
        </p>
        <p className="mvx-admin-muted" style={{ margin: "0 0 8px" }}>
          An expired or invalid key never stops the platform: from the day after the expiry date it runs the Community Edition, every tenant too, and reports the reason here. Nothing is deleted.
          Enterprise and commercial source lives under <code>ee/</code> in the repository and is unlocked at runtime by the key.
        </p>
        <p className="mvx-admin-muted" style={{ margin: 0 }}>
          The key implements an accepted order and names it above; the rights themselves are the order&apos;s and its agreement&apos;s, not the key&apos;s.
          Its features come from the feature schedule it was sold under, so features released later are not added to it.
        </p>
      </Card>
    </div>
  );
}

/** Paste a key and install it, or remove the one installed here. */
function InstallKey({ source }: { source: string }) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [key, setKey] = useState("");
  // What depends on the edition: the account menu, every feature gate, the
  // editions a tenant can be on.
  const refresh = () => { void qc.invalidateQueries({ queryKey: ["license"] }); void qc.invalidateQueries({ queryKey: ["admin-plans"] }); };
  const install = useMutation({ mutationFn: () => api.installLicense(key), onSuccess: () => { setKey(""); refresh(); } });
  const remove = useMutation({ mutationFn: api.removeLicense, onSuccess: refresh });

  if (source === "env" || source === "file") {
    return (
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 8 }}>Install a license key</div>
        <p className="mvx-admin-muted" style={{ margin: 0 }} data-testid="license-from-config">
          This deployment&apos;s key is set in the server&apos;s configuration (<code>{source === "env" ? "MAVERICKS_LICENSE_KEY" : "MAVERICKS_LICENSE_FILE"}</code>),
          and that one is in force. Renew or change it there and restart the gateway, or remove that setting to install keys on this page.
        </p>
      </Card>
    );
  }
  return (
    <Card>
      <div style={{ fontWeight: 700, marginBottom: 8 }}>{source === "console" ? "Install another license key" : "Install a license key"}</div>
      <p className="mvx-admin-muted" style={{ margin: "0 0 10px" }}>
        Paste the key you received for this deployment: one line starting <code>MVX1.</code> The edition changes at once, with no restart.
        New tenants then start on the key&apos;s edition; existing tenants keep theirs until moved under Applications › Change edition.
      </p>
      <Field label="License key">
        <Textarea value={key} onChange={(e) => setKey(e.target.value)} rows={3} placeholder="MVX1.…" aria-label="License key" spellCheck={false} style={{ fontFamily: "var(--font-mono, monospace)" }} />
      </Field>
      <div style={{ display: "flex", gap: 8, marginTop: 10, flexWrap: "wrap" }}>
        <Button variant="primary" disabled={!key.trim()} loading={install.isPending} loadingLabel="Installing…" onClick={() => install.mutate()}>Install key</Button>
        {source === "console" && (
          <Button variant="danger" loading={remove.isPending} loadingLabel="Removing…" onClick={() => confirm({
            title: "Remove the license key?",
            body: "The deployment runs the Community Edition from now on: the paid features stop, nothing they stored is deleted, and every tenant moves to the Community Edition.",
            confirmLabel: "Remove key", destructive: true, onConfirm: () => remove.mutate(),
          })}>Remove key</Button>
        )}
      </div>
      {install.isError && <InlineAlert tone="danger">{(install.error as Error).message}</InlineAlert>}
      {install.isSuccess && <InlineAlert tone="success">Installed: the deployment runs the <Txt>{EDITION_LABELS[install.data.edition]}</Txt><Txt> Edition.</Txt></InlineAlert>}
      {remove.isError && <InlineAlert tone="danger">{(remove.error as Error).message}</InlineAlert>}
      {confirmElement}
    </Card>
  );
}
