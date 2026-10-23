import React from "react";
import { Loader2 } from "lucide-react";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger" | "dangerSecondary";
export type ButtonSize = "sm" | "md" | "lg";

interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  icon?: React.ReactNode;
  leadingIcon?: React.ReactNode;
  trailingIcon?: React.ReactNode;
  loadingLabel?: string;
}

export function Button({
  variant = "secondary",
  size = "md",
  loading = false,
  icon,
  leadingIcon,
  trailingIcon,
  loadingLabel,
  disabled,
  children,
  className,
  ...rest
}: ButtonProps) {
  const leftIcon = loading
    ? <Loader2 size={14} className="mvx-button__spinner" aria-hidden="true" />
    : leadingIcon ?? icon;

  return (
    <button
      {...rest}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      className={["mvx-button", `mvx-button--${variant}`, `mvx-button--${size}`, className].filter(Boolean).join(" ")}
    >
      {leftIcon}
      {/* The label sits in an element of its own, replaced whole when the
          loading label takes its place. A page translator (a browser's
          "translate this page") swaps a bare text node for <font> elements
          of its own; the spinner then went in before a text node that was
          no longer the button's child, and React's NotFoundError blanked the
          console (Yandex Browser, reported 2026-10-23). display: contents
          keeps the button's layout as it was. */}
      <span key={loading && loadingLabel ? "loading" : "idle"} className="mvx-button__label">
        {loading && loadingLabel ? loadingLabel : children}
      </span>
      {!loading && trailingIcon}
    </button>
  );
}
