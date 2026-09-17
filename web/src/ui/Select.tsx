import React from "react";

interface SelectProps extends React.SelectHTMLAttributes<HTMLSelectElement> {
  error?: boolean;
}

export function Select({ error, style, children, ...rest }: SelectProps) {
  return (
    <select
      {...rest}
      className={["mvx-select", error ? "mvx-select--error" : "", rest.className].filter(Boolean).join(" ")}
      style={style}
    >
      {children}
    </select>
  );
}
