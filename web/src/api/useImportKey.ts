import { useRef } from "react";
import { newImportKey } from "./client";

/**
 * One Idempotency-Key per import a person sends. The same content sent
 * again — a button clicked again after a 502 or a closed connection — goes
 * under the key it was first sent with, so the server applies it once. A
 * success frees the key: the next run, even of the same content (a sheet
 * synced again, a file loaded again on purpose), is a run of its own.
 * Without a key, a saved integration's run sent again created every record
 * twice and added incremental values twice (2026-11-09).
 */
export function useImportKey(): { keyFor: (content: string) => string; done: () => void } {
  const ref = useRef<{ content: string; key: string } | null>(null);
  return {
    keyFor: (content) => {
      if (ref.current?.content !== content) ref.current = { content, key: newImportKey() };
      return ref.current.key;
    },
    done: () => { ref.current = null; },
  };
}
