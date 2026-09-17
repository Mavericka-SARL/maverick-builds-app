import React, { useCallback, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CheckCircle2, AlertCircle, Info, X } from "lucide-react";
import { IconButton } from "./IconButton";
import { ToastContext, type ToastVariant } from "./ToastContext";

interface Toast {
  id: number;
  message: string;
  variant: ToastVariant;
}

const ICONS: Record<ToastVariant, React.ReactNode> = {
  success: <CheckCircle2 size={16} style={{ color: "var(--color-success)", flexShrink: 0 }} />,
  error:   <AlertCircle size={16} style={{ color: "var(--color-danger)", flexShrink: 0 }} />,
  info:    <Info size={16} style={{ color: "var(--color-brand-500)", flexShrink: 0 }} />,
};

let counter = 0;

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const timers = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map());

  const dismiss = useCallback((id: number) => {
    setToasts((t) => t.filter((x) => x.id !== id));
    clearTimeout(timers.current.get(id));
    timers.current.delete(id);
  }, []);

  const toast = useCallback((message: string, variant: ToastVariant = "success") => {
    const id = ++counter;
    setToasts((t) => [...t, { id, message, variant }]);
    timers.current.set(id, setTimeout(() => dismiss(id), 3500));
  }, [dismiss]);

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      {createPortal(
        <div style={{ position: "fixed", bottom: 24, right: 24, zIndex: 2000, display: "flex", flexDirection: "column", gap: 8, alignItems: "flex-end" }}>
          {toasts.map((t) => (
            <div
              key={t.id}
              style={{
                display: "flex",
                alignItems: "center",
                gap: 10,
                background: "var(--color-surface)",
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-button)",
                boxShadow: "var(--shadow-card-hover)",
                padding: "10px 12px",
                fontSize: 13,
                color: "var(--color-text)",
                minWidth: 240,
                maxWidth: 360,
                animation: "slideIn 0.15s ease-out",
              }}
            >
              {ICONS[t.variant]}
              <span style={{ flex: 1 }}>{t.message}</span>
              <IconButton aria-label="Dismiss" size={24} onClick={() => dismiss(t.id)}>
                <X size={12} />
              </IconButton>
            </div>
          ))}
        </div>,
        document.body,
      )}
    </ToastContext.Provider>
  );
}
