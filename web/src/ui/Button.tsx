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
      {loading && loadingLabel ? loadingLabel : children}
      {!loading && trailingIcon}
    </button>
  );
}
