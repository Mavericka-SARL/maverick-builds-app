// The first change to the live revision in a session asks first (decided
// 2026-10-06): Build works in whichever revision is selected, and with the
// live one selected an edit reaches business users at once, with nothing on
// screen saying so. The developer console installs the guard while its
// working revision is the live one; apiFetch consults it before a request
// that changes revision content.

type Guard = () => Promise<boolean>;

let guard: Guard | null = null;

export function setLiveEditGuard(g: Guard | null): void {
  guard = g;
}

// Requests that change what a revision holds. Revisions themselves (making
// a new one, the recommended way out) and the AI Developer's own draft are
// not edits of the live revision.
const REVISION_CONTENT = /^\/api\/developer\/(metrics|dimensions|grids|dashboards|folders|form-integrations|workflows|integrations|integration-connections|model-links)(\/|\?|$)/;

// Posts that check or run what is there and change no definition: a
// validation, a connection test, a workflow test run, an integration run
// (a data load, as on any revision).
const NO_DEFINITION_CHANGE = /\/(validate|test|test-run|run)(\?|$)/;

export async function liveEditAllowed(method: string | undefined, path: string): Promise<boolean> {
  if (!guard || !method || method.toUpperCase() === "GET" || !REVISION_CONTENT.test(path) || NO_DEFINITION_CHANGE.test(path)) return true;
  return guard();
}
