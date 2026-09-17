import React, { useState } from "react";
import { ConfirmDialog } from "./ConfirmDialog";
import type { ConfirmState } from "./ConfirmDialog";

const CLOSED: ConfirmState = {
  open: false, title: "", body: "", confirmLabel: "Confirm", destructive: true, onConfirm: () => {},
};

export function useConfirm() {
  const [state, setState] = useState<ConfirmState>(CLOSED);

  const confirm = (opts: Omit<ConfirmState, "open">) =>
    setState({ ...opts, open: true });

  const close = () => setState(CLOSED);

  const element = React.createElement(ConfirmDialog, {
    open: state.open,
    title: state.title,
    body: state.body,
    confirmLabel: state.confirmLabel,
    destructive: state.destructive,
    typeToConfirm: state.typeToConfirm,
    onConfirm: () => { state.onConfirm(); close(); },
    onCancel: close,
  });

  return { confirm, confirmElement: element };
}
