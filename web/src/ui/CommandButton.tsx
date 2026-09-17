import React from "react";

interface CommandButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  /** Widget-configured background color; falls back to brand. */
  color?: string;
}

/**
 * Fill-parent action button used by dashboard command widgets
 * (automation trigger, workflow start, integration import).
 */
export function CommandButton({ color, className, style, ...rest }: CommandButtonProps) {
  return (
    <button
      type="button"
      {...rest}
      className={["mvx-command-button", className].filter(Boolean).join(" ")}
      style={{ background: color, ...style }}
    />
  );
}
