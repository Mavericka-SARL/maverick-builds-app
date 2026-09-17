import React, { useState } from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";
import { Button } from "./Button";
import { IconButton } from "./IconButton";

interface ConfirmDialogProps {
  open: boolean;
  title: string;
  body: React.ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** When true the confirm button uses danger styling */
  destructive?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  /** Require the user to type this string before confirming */
  typeToConfirm?: string;
}

export function ConfirmDialog({
  open,
  title,
  body,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  destructive = true,
  onConfirm,
  onCancel,
  typeToConfirm,
}: ConfirmDialogProps) {
  const [typed, setTyped] = useState("");

  if (!open) return null;

  const canConfirm = typeToConfirm ? typed.trim() === typeToConfirm : true;

  return createPortal(
    <div
      style={{
        position: "fixed",
        inset: 0,
        zIndex: 1100,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        background: "rgba(0,0,0,0.4)",
      }}
    >
      <div
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="confirm-title"
        aria-describedby="confirm-body"
        style={{
          background: "var(--color-surface)",
          borderRadius: "var(--radius-card)",
          boxShadow: "var(--shadow-modal)",
          width: 420,
          maxWidth: "calc(100vw - 32px)",
        }}
        onKeyDown={(e) => { if (e.key === "Escape") onCancel(); }}
      >
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", padding: "16px 20px", borderBottom: "1px solid var(--color-border)" }}>
          <span id="confirm-title" style={{ fontSize: 15, fontWeight: 600, color: "var(--color-text)" }}>
            {title}
          </span>
          <IconButton aria-label="Close" title="Close" onClick={onCancel}>
            <X size={16} />
          </IconButton>
        </div>

        <div id="confirm-body" style={{ padding: "16px 20px", fontSize: 13, color: "var(--color-text-muted)", lineHeight: 1.6 }}>
          {body}
          {typeToConfirm && (
            <div style={{ marginTop: 12 }}>
              <div style={{ fontSize: 12, color: "var(--color-text-muted)", marginBottom: 6 }}>
                Type <strong style={{ color: "var(--color-text)" }}>{typeToConfirm}</strong> to confirm
              </div>
              <input
                autoFocus
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                style={{
                  width: "100%",
                  height: 32,
                  padding: "0 10px",
                  border: "1px solid var(--color-border)",
                  borderRadius: "var(--radius-input)",
                  fontSize: 13,
                  fontFamily: "var(--font-sans)",
                  outline: "none",
                  boxSizing: "border-box",
                }}
              />
            </div>
          )}
        </div>

        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, padding: "12px 20px", borderTop: "1px solid var(--color-border)" }}>
          <Button variant="secondary" onClick={onCancel} autoFocus={!typeToConfirm}>
            {cancelLabel}
          </Button>
          <Button
            variant={destructive ? "danger" : "primary"}
            onClick={onConfirm}
            disabled={!canConfirm}
          >
            {confirmLabel}
          </Button>
        </div>
      </div>
    </div>,
    document.body,
  );
}

// Lightweight hook for managing a single confirm dialog per component
export interface ConfirmState {
  open: boolean;
  title: string;
  body: React.ReactNode;
  confirmLabel: string;
  destructive?: boolean;
  typeToConfirm?: string;
  onConfirm: () => void | Promise<void>;
}
