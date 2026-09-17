import React from "react";

interface IconButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  "aria-label": string;
  danger?: boolean;
  size?: number;
}

export function IconButton({ danger = false, size = 32, style, className, ...rest }: IconButtonProps) {
  return (
    <button
      {...rest}
      className={["mvx-icon-button", danger ? "mvx-icon-button--danger" : "", className].filter(Boolean).join(" ")}
      style={{ width: size, height: size, minWidth: size, ...style }}
    />
  );
}
