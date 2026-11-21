import { useQuery } from "@tanstack/react-query";
import { api, type FormDef } from "../../api/client";
import { Dialog, InlineAlert, LoadingState } from "../../ui";
import { RecordDialog } from "./RecordLinks";

/**
 * The record a notification names (a person field with notify: "you are its
 * Assignee"), opened over whatever page is showing. A record of another
 * model, or one the person no longer sees, says so instead.
 */
export function NotifiedRecord({ recordId, revisionId, onClose }: { recordId: string; revisionId: string; onClose: () => void }) {
  const { data: record, error, isLoading } = useQuery({
    queryKey: ["records", "one", recordId],
    queryFn: () => api.getRecord(recordId),
    retry: false,
  });
  const { data: formsRaw, isLoading: formsLoading } = useQuery({
    queryKey: ["forms", revisionId],
    queryFn: () => api.listForms(revisionId || undefined),
  });
  const forms = (formsRaw ?? []) as FormDef[];
  const form = record ? forms.find(f => f.id === record.form_id) : undefined;
  if (record && form) {
    return <RecordDialog form={form} forms={forms} record={record} revisionId={revisionId} onClose={onClose} />;
  }
  return (
    <Dialog open onClose={onClose} title="Record" width={480}>
      {isLoading || formsLoading ? <LoadingState label="Opening the record…" />
        : error ? <InlineAlert tone="warning">This record is not one you can open any more.</InlineAlert>
          : <InlineAlert tone="info">This record belongs to another model: open that model under Models, then the record from its dashboard.</InlineAlert>}
    </Dialog>
  );
}
