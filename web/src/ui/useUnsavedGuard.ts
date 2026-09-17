import { useEffect } from "react";
import { useConfirm } from "./useConfirm";

/**
 * Protects unsaved edits from being lost by a navigation the user didn't
 * think of as leaving.
 *
 * Two escape routes existed and neither was covered:
 *
 *  - Leaving the page entirely (reload, close, Back) — nothing anywhere in
 *    the app registered a `beforeunload` handler, so a dashboard layout or a
 *    set of access-rule edits vanished silently.
 *  - Switching context inside the app — changing the selected user on the
 *    access-rules panel reset the draft outright (`setDraft({})`), so the
 *    work was gone with no prompt at all.
 *
 * `guard` wraps an in-app transition: it runs immediately when there's
 * nothing to lose, and asks first when there is. It deliberately routes
 * through the shared ConfirmDialog rather than window.confirm — native
 * dialogs were removed from this codebase on purpose, and the accessibility
 * gate asserts confirmations are real alertdialogs.
 *
 * The browser-level prompt can't be styled or customised: browsers show
 * their own generic wording for beforeunload, and only when the user has
 * interacted with the page. That's a platform limit, not a stylistic choice.
 *
 * Render `guardElement` somewhere in the component that uses this.
 */
export function useUnsavedGuard(isDirty: boolean) {
  const { confirm, confirmElement } = useConfirm();

  useEffect(() => {
    if (!isDirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      // preventDefault is the modern signal; returnValue is kept for the
      // browsers that still require it.
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [isDirty]);

  const guard = (onProceed: () => void) => {
    if (!isDirty) {
      onProceed();
      return;
    }
    confirm({
      title: "Discard unsaved changes?",
      body: "Your edits haven't been saved yet. Leaving now discards them.",
      confirmLabel: "Discard changes",
      destructive: true,
      onConfirm: onProceed,
    });
  };

  return { guard, guardElement: confirmElement };
}
