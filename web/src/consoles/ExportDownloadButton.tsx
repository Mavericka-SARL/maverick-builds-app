import { useState } from "react";
import { Download } from "lucide-react";
import { api } from "../api/client";
import { downloadBlob } from "./business/blobUtils";
import { Button } from "../ui";

// Downloads a "file_export" integration. The server builds the file for the
// person clicking, from their own view of the grid, so the same button is
// right in the developer console, the AI panel and a business dashboard.
export function ExportDownloadButton({
  integrationId,
  label = "Download",
  size = "sm",
  variant,
}: {
  integrationId: string;
  label?: string;
  size?: "sm" | "md";
  variant?: "primary" | "secondary";
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const download = async () => {
    setBusy(true);
    setError(null);
    try {
      const { blob, filename } = await api.downloadIntegrationExport(integrationId);
      downloadBlob(blob, filename);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Download failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
      <Button size={size} variant={variant} leadingIcon={<Download size={13} />} loading={busy} loadingLabel="Preparing…" onClick={download}>
        {label}
      </Button>
      {error && <span className="mvx-admin-error" style={{ fontSize: 12 }}>{error}</span>}
    </span>
  );
}
