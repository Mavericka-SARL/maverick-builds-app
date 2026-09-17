import React from "react";

interface TextInputProps extends React.InputHTMLAttributes<HTMLInputElement> {
  error?: boolean;
}

export function TextInput({ error, style, ...rest }: TextInputProps) {
  return (
    <input
      {...rest}
      className={["mvx-input", error ? "mvx-input--error" : "", rest.className].filter(Boolean).join(" ")}
      style={style}
    />
  );
}
