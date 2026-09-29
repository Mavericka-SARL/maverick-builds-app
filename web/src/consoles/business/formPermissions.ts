// What Run › Forms and the dashboard form widget offer on a form as a whole,
// read from the permissions GET /api/forms serves on each form (the server
// computes them from the scope that authorises the sync and the create).
// A form without permissions — an older server — gets nothing offered.
import type { ApiError, FormDef } from "../../api/client";

/** "Sync to grid" is offered only to a caller the server lets sync. */
export function canSyncForm(form: FormDef | null | undefined): boolean {
  return !!form?.permissions?.sync;
}

/** The statuses a new record of form may be created in, in lifecycle order. */
export function createStatusesOf(form: FormDef | null | undefined): string[] {
  return form?.permissions?.create_statuses ?? [];
}

/**
 * The status a new record is created in: the user's pick while the server
 * still offers it, otherwise the first status it offers (a draft).
 */
export function createStatusFor(form: FormDef | null | undefined, picked: string | null): string | undefined {
  const statuses = createStatusesOf(form);
  return picked && statuses.includes(picked) ? picked : statuses[0];
}

export type SyncMessage = { tone: "success" | "danger"; text: string };

/** What a failed sync tells the user; a 403 is the server's administrator rule. */
export function syncFailure(err: Error): SyncMessage {
  if ((err as Partial<ApiError>).status === 403) {
    return { tone: "danger", text: "Only an administrator of this application can sync." };
  }
  return { tone: "danger", text: `Sync failed: ${err.message}` };
}

/**
 * Whether a failed sync means the permissions the forms list holds are out of
 * date (the server refuses, 403, or no longer shows the form, 404): the list
 * is then reloaded, so "Sync to grid" follows the server's current answer.
 */
export function syncRefused(err: Error): boolean {
  const status = (err as Partial<ApiError>).status;
  return status === 403 || status === 404;
}
