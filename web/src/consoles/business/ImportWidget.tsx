import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import * as XLSX from "xlsx";
import { Download, Upload } from "lucide-react";
import { api, type DemoContext, type GridData, type ImportModeParam, type ImportRowError, type ApiError, type ImportJob } from "../../api/client";
import { SectionHeader, Button, Field, Select, Textarea, InlineAlert, StatusBadge, type DesignTone } from "../../ui";
import { downloadBlob, fileToBase64 } from "./blobUtils";

const STATUS_LABEL: Record<number, string> = { 0: "Unknown", 1: "Validating", 2: "Staged", 3: "Committed", 4: "Failed", 5: "Aborted" };
const IMPORT_STATUS_TONE: Record<number, DesignTone> = { 1: "warning", 2: "draft", 3: "success", 4: "danger", 5: "neutral" };

// buildImportTemplateWorkbook generates a downloadable .xlsx template whose
// header row is [this grid's dimension names] + [this grid's writable input
// metric names] — exactly the column-name contract importpkg.ResolveRows
// expects server-side, generated generically from whatever the caller's
// visible grid actually contains (never a hardcoded column list). Grid
// security scoping (see internal/gateway grid()) means a cost-center
// manager's template already only lists their own dimension members as the
// example row's codes.
function buildImportTemplateWorkbook(grid: GridData): Blob {
  const dimNames = grid.dimensions.map(d => d.name);
  const metricNames = grid.metrics.filter(m => m.is_input && !m.readonly).map(m => m.name);
  const header = [...dimNames, ...metricNames];
  const exampleRow = [
    ...grid.dimensions.map(d => d.members.find(m => !d.members.some(o => o.parent_code === m.code))?.code ?? d.members[0]?.code ?? ""),
    ...metricNames.map(() => 0),
  ];
  const ws = XLSX.utils.aoa_to_sheet([header, exampleRow]);
  const wb = XLSX.utils.book_new();
  XLSX.utils.book_append_sheet(wb, ws, "Import");
  const out = XLSX.write(wb, { type: "array", bookType: "xlsx" });
  return new Blob([out], { type: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" });
}

export function ImportWidget({ gridDefId, ctx }: { gridDefId: string; ctx: DemoContext }) {
  const qc = useQueryClient();
  const [csv, setCsv] = useState("");
  const [mode, setMode] = useState<ImportModeParam>("replace");
  const [rowErrors, setRowErrors] = useState<ImportRowError[] | null>(null);
  const fileInputRef = React.useRef<HTMLInputElement>(null);

  // Used both to build the template and to show the manager which
  // dimensions/metrics an upload will map against — already scoped to only
  // what this caller can see (grid()'s access-rule filtering) AND to this
  // widget's configured grid, since import placement is dashboard-scoped.
  const { data: grid } = useQuery({
    queryKey: ["grid", ctx.revision_id, gridDefId],
    queryFn: () => api.getGrid(ctx.revision_id, gridDefId),
  });

  const inv = () => qc.invalidateQueries({ queryKey: ["import-jobs"] });

  const uploadCsv = useMutation({
    mutationFn: () => api.uploadImport(csv, ctx.revision_id, mode),
    onSuccess: () => { inv(); setCsv(""); setRowErrors(null); },
    onError: (e) => setRowErrors((e as ApiError).body?.errors as ImportRowError[] | undefined ?? null),
  });

  const uploadXlsx = useMutation({
    mutationFn: async (file: File) => api.uploadImportXlsx(await fileToBase64(file), ctx.revision_id, mode),
    onSuccess: () => inv(),
    onError: (e) => setRowErrors((e as ApiError).body?.errors as ImportRowError[] | undefined ?? null),
    onMutate: () => setRowErrors(null),
  });

  const { data: jobs = [] } = useQuery({
    queryKey: ["import-jobs"],
    queryFn: api.getImportJobs,
    refetchInterval: 10_000,
  });

  const lastError = (uploadCsv.error ?? uploadXlsx.error) as Error | undefined;
  const lastResult = uploadCsv.data ?? uploadXlsx.data;

  return (
    <div style={{ maxWidth: 720 }}>
      <SectionHeader
        title="Import"
        subtitle={<>Upload a native <code>.xlsx</code> workbook or paste CSV for revision <strong>{ctx.revision}</strong>. Column headers are dimension or metric <em>names</em> from this grid — no internal IDs required.</>}
      />

      <div style={{ display: "flex", gap: 12, alignItems: "center", marginBottom: 16, flexWrap: "wrap" }}>
        <Button
          leadingIcon={<Download size={14} />}
          disabled={!grid}
          onClick={() => grid && downloadBlob(buildImportTemplateWorkbook(grid), "import-template.xlsx")}
        >
          Download .xlsx template
        </Button>
        <Button
          variant="primary"
          leadingIcon={<Upload size={14} />}
          loading={uploadXlsx.isPending}
          loadingLabel="Uploading…"
          onClick={() => fileInputRef.current?.click()}
        >
          Upload .xlsx
        </Button>
        <input
          ref={fileInputRef}
          type="file"
          accept=".xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
          style={{ display: "none" }}
          onChange={(e) => {
            const file = e.target.files?.[0];
            e.target.value = "";
            if (file) uploadXlsx.mutate(file);
          }}
        />
        <Field label="Mode" style={{ marginBottom: 0 }}>
          <Select value={mode} onChange={(e) => setMode(e.target.value as ImportModeParam)} aria-label="Import mode" style={{ width: 160 }}>
            <option value="replace">Replace listed cells</option>
            <option value="incremental">Add to existing</option>
            <option value="full_reload">Full reload (danger)</option>
          </Select>
        </Field>
      </div>

      <details>
        <summary className="mvx-admin-muted" style={{ cursor: "pointer", fontSize: 13, marginBottom: 8 }}>Or paste CSV</summary>
        <Textarea
          value={csv}
          onChange={(e) => setCsv(e.target.value)}
          placeholder={grid ? `${[...grid.dimensions.map(d => d.name), ...grid.metrics.filter(m => m.is_input).map(m => m.name)].join(",")}\n...` : "metric_id,value,..."}
          rows={6}
          style={{ width: "100%", fontFamily: "var(--font-mono)", fontSize: 12, lineHeight: 1.5, marginTop: 8 }}
        />
        <div style={{ marginTop: 8 }}>
          <Button
            variant="primary"
            disabled={!csv.trim()}
            loading={uploadCsv.isPending}
            loadingLabel="Uploading…"
            onClick={() => uploadCsv.mutate()}
          >
            Upload & Commit CSV
          </Button>
        </div>
      </details>

      {lastError && (
        <div style={{ marginTop: 16 }}>
          <InlineAlert tone="danger">{lastError.message}</InlineAlert>
          {rowErrors && rowErrors.length > 0 && (
            <div className="mvx-table-wrap" style={{ marginTop: 8 }}>
              <table className="mvx-table mvx-table--compact">
                <thead><tr><th>Row</th><th>Column</th><th>Problem</th><th>Value</th></tr></thead>
                <tbody>
                  {rowErrors.map((e, i) => (
                    <tr key={i}>
                      <td>{e.row}</td>
                      <td className="mvx-admin-mono">{e.column}</td>
                      <td>{e.message}</td>
                      <td className="mvx-admin-mono mvx-admin-muted">{e.raw_value ?? ""}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
      {lastResult && !lastError && (
        <div style={{ marginTop: 16 }}>
          <InlineAlert tone="success">{lastResult.valid_rows} cell{lastResult.valid_rows === 1 ? "" : "s"} imported</InlineAlert>
        </div>
      )}

      {(jobs as ImportJob[]).length > 0 && (
        <div style={{ marginTop: 32 }}>
          <SectionHeader title="Recent Jobs" />
          <div className="mvx-table-wrap">
            <table className="mvx-table mvx-table--compact">
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Revision</th>
                  <th style={{ textAlign: "right" }}>Total</th>
                  <th style={{ textAlign: "right" }}>Valid</th>
                  <th style={{ textAlign: "right" }}>Errors</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {(jobs as ImportJob[]).map((j) => (
                  <tr key={j.id}>
                    <td className="mvx-admin-mono mvx-admin-muted">{j.id.slice(0, 8)}</td>
                    <td>{j.revision_name}</td>
                    <td style={{ textAlign: "right" }}>{j.total_rows}</td>
                    <td style={{ textAlign: "right", color: "var(--color-live)" }}>{j.valid_rows}</td>
                    <td style={{ textAlign: "right", color: j.error_rows > 0 ? "var(--color-danger)" : "var(--color-text-subtle)" }}>{j.error_rows}</td>
                    <td>
                      <StatusBadge tone={IMPORT_STATUS_TONE[j.status] ?? "neutral"}>
                        {STATUS_LABEL[j.status] ?? String(j.status)}
                      </StatusBadge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
