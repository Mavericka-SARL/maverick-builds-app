import { Check, Lock } from "lucide-react";
import { Card, InlineAlert, StatusBadge } from "../../ui";
import { EDITION_LABELS, licenseDate as fmtDate, useLicense } from "../../license/useLicense";

const STATE_TONE = { community: "neutral", active: "success", transition: "warning", expired: "warning", invalid: "danger" } as const;
const STATE_LABEL = { community: "No license key", active: "Active", transition: "Expired · transition", expired: "Expired", invalid: "Invalid" } as const;

function daysLeft(iso?: string): string {
  if (!iso) return "";
  const ms = new Date(iso).getTime() - Date.now();
  if (Number.isNaN(ms)) return "";
  const days = Math.ceil(ms / 86_400_000);
  if (days < 0) return ` (${-days} day${days === -1 ? "" : "s"} ago)`;
  if (days === 0) return " (today)";
  return ` (in ${days} day${days === 1 ? "" : "s"})`;
}

/**
 * Platform › License: which edition this deployment runs, why, and what each
 * other edition would add. Read-only on purpose — the key is deployment
 * configuration (an environment variable or a file on the gateway), not
 * something stored in the database, so applying one is an operator action
 * described here rather than a form.
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
    ["Expires", lic.expires_at ? `${fmtDate(lic.expires_at)}${daysLeft(lic.expires_at)}` : "—"],
    ...(lic.transition_ends_at ? [["Transition ends", `${fmtDate(lic.transition_ends_at)}${daysLeft(lic.transition_ends_at)}`] as [string, string]] : []),
    ["Key source", lic.source === "env" ? "MAVERICKS_LICENSE_KEY" : lic.source === "file" ? "MAVERICKS_LICENSE_FILE" : "none configured"],
  ];

  return (
    <div className="mvx-admin-stack" data-testid="license-tab">
      <Card>
        <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 12 }}>
          <span style={{ fontSize: 17, fontWeight: 700 }}>{EDITION_LABELS[lic.edition]} edition</span>
          <StatusBadge tone={STATE_TONE[lic.state]}>{STATE_LABEL[lic.state]}</StatusBadge>
        </div>
        {lic.state === "invalid" && (
          <InlineAlert tone="danger">The configured license key could not be verified: {lic.error}. The deployment runs the Community edition until a valid key is installed.</InlineAlert>
        )}
        {lic.state === "active" && lic.renewal_due && (
          <InlineAlert tone="warning">The license key expires on {fmtDate(lic.expires_at)}. Install the renewed key before then; after expiry the paid features are read and export only for 30 days, then the deployment runs the Community edition.</InlineAlert>
        )}
        {lic.state === "transition" && (
          <InlineAlert tone="warning">The license key expired on {fmtDate(lic.expires_at)}. Until {fmtDate(lic.transition_ends_at)} the paid features keep working as configured and can be read, exported and switched off, but not changed. After that the deployment runs the Community edition. No data is deleted either way.</InlineAlert>
        )}
        {lic.state === "expired" && (
          <InlineAlert tone="warning">The license key expired on {fmtDate(lic.expires_at)} and its transition period has ended. The deployment runs the Community edition until a new key is installed; nothing the paid features stored was deleted.</InlineAlert>
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

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 8 }}>Applying a license key</div>
        <p style={{ margin: "0 0 8px" }}>
          A key is issued per deployment and verified offline. Set it on the gateway as the environment variable
          <code> MAVERICKS_LICENSE_KEY</code> (the token itself) or <code>MAVERICKS_LICENSE_FILE</code> (a path to a file holding it),
          then restart the gateway. This page and the account menu reflect the new edition immediately after the restart.
        </p>
        <p className="mvx-admin-muted" style={{ margin: "0 0 8px" }}>
          An expired or invalid key never stops the platform: it runs the Community edition and reports the reason here.
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
