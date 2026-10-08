import { useEffect, useState } from "react";
import type { ApiSFTPSource, FileReshape } from "../../../api/client";
import { Button, Field, NumberInput, Select, StatusBadge, TextInput } from "../../../ui";
import { sshFingerprint } from "./apiIntegrationTypes";

// The SFTP side of the Request step: which server, which file, and how to
// read it. Port 22 only; the file stays on the server after each run. The
// host key is trusted from a test run's result (Test & response step), and
// changing the server forgets it.
export function SFTPSourceFields({ source, onSource }: {
  source: ApiSFTPSource;
  onSource: (s: ApiSFTPSource) => void;
}) {
  const patch = (p: Partial<ApiSFTPSource>) => onSource({ ...source, ...p });
  const reshape: FileReshape = source.reshape ?? {};
  const patchReshape = (p: Partial<FileReshape>) => {
    const next: FileReshape = { ...reshape, ...p };
    if (!next.header_row || next.header_row <= 1) delete next.header_row;
    if (!next.delimiter || next.delimiter === ",") delete next.delimiter;
    patch({ reshape: Object.keys(next).length > 0 ? next : undefined });
  };
  const [fingerprint, setFingerprint] = useState("");
  useEffect(() => {
    let live = true;
    if (source.host_key) void sshFingerprint(source.host_key).then(fp => { if (live) setFingerprint(fp); });
    return () => { live = false; };
  }, [source.host_key]);

  return (
    <div style={{ display: "grid", gap: 12, maxWidth: 640 }}>
      <Field label="Server" required description="Server name or address only. Port 22.">
        <TextInput value={source.host} aria-label="SFTP server" placeholder="sftp.example.com"
          onChange={e => patch({ host: e.target.value, host_key: undefined })} />
      </Field>

      <Field label="File">
        <Select value={source.select} aria-label="Which file"
          onChange={e => {
            const select = e.target.value as ApiSFTPSource["select"];
            patch(select === "fixed"
              ? { select, path: source.path ?? "", folder: undefined, pattern: undefined }
              : { select, path: undefined, folder: source.folder ?? "", pattern: source.pattern ?? "" });
          }}>
          <option value="fixed">A fixed file</option>
          <option value="newest">The newest file in a folder that matches a pattern</option>
        </Select>
      </Field>
      {source.select === "fixed" ? (
        <Field label="File path" required description="A .csv, .xlsx or .xlsm file. A path without a leading / starts in the login folder.">
          <TextInput value={source.path ?? ""} aria-label="File path" placeholder="/exports/sales.xlsx"
            onChange={e => patch({ path: e.target.value })} />
        </Field>
      ) : (
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))", gap: 12 }}>
          <Field label="Folder" description="Empty = the login folder">
            <TextInput value={source.folder ?? ""} aria-label="Folder" placeholder="/exports"
              onChange={e => patch({ folder: e.target.value })} />
          </Field>
          <Field label="File name pattern" required description="* matches any characters, ? one. Of the matching .csv, .xlsx and .xlsm files, the most recently modified is read.">
            <TextInput value={source.pattern ?? ""} aria-label="File name pattern" placeholder="sales_*.xlsx"
              onChange={e => patch({ pattern: e.target.value })} />
          </Field>
        </div>
      )}

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(160px, 1fr))", gap: 12 }}>
        <Field label="Sheet" description="Workbook sheet; empty = the first">
          <TextInput value={source.sheet ?? ""} aria-label="Sheet"
            onChange={e => patch({ sheet: e.target.value || undefined })} />
        </Field>
        <Field label="Header row" description="The row with the column names">
          <NumberInput value={reshape.header_row ?? 1} min={1} aria-label="Header row"
            onChange={e => patchReshape({ header_row: Number(e.target.value) || 1 })} />
        </Field>
        <Field label="CSV delimiter">
          <Select value={reshape.delimiter ?? ","} aria-label="CSV delimiter"
            onChange={e => patchReshape({ delimiter: e.target.value })}>
            <option value=",">Comma ,</option>
            <option value=";">Semicolon ;</option>
            <option value={"\t"}>Tab</option>
            <option value="|">Pipe |</option>
          </Select>
        </Field>
      </div>

      <Field label="Server host key">
        {source.host_key ? (
          <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
            <StatusBadge tone="success">Trusted</StatusBadge>
            <code style={{ fontSize: 12 }}>{fingerprint || source.host_key.split(/\s+/)[0]}</code>
            <Button variant="secondary" size="sm" onClick={() => patch({ host_key: undefined })}>Forget</Button>
          </div>
        ) : (
          <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 12 }}>
            Not trusted yet. The first test reads the server&apos;s key and shows its fingerprint for you to compare
            and trust. No file is read before that.
          </p>
        )}
      </Field>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 12 }}>
        The file stays on the server after each run. A scheduled run skips a file that has not changed since the
        last import; Run now always imports.
      </p>
    </div>
  );
}
