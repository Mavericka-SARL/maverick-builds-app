import type React from "react";
import { TextInput } from "./TextInput";

interface TextareaProps extends React.TextareaHTMLAttributes<HTMLTextAreaElement> {
  error?: boolean;
  ref?: React.Ref<HTMLTextAreaElement>;
}

interface CheckboxProps extends Omit<React.InputHTMLAttributes<HTMLInputElement>, "type"> {
  label: React.ReactNode;
}

interface SwitchProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
  "aria-label": string;
}

export function NumberInput(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return <TextInput type="number" inputMode="decimal" {...props} />;
}

export function Textarea({ error, className, ...rest }: TextareaProps) {
  return (
    <textarea
      {...rest}
      className={["mvx-textarea", error ? "mvx-textarea--error" : "", className].filter(Boolean).join(" ")}
    />
  );
}

export function Checkbox({ label, className, ...rest }: CheckboxProps) {
  return (
    <label className={["mvx-checkbox-row", className].filter(Boolean).join(" ")}>
      <input {...rest} type="checkbox" className="mvx-checkbox" />
      <span>{label}</span>
    </label>
  );
}

export function Switch({ checked, onChange, disabled, "aria-label": ariaLabel }: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-label={ariaLabel}
      aria-checked={checked}
      disabled={disabled}
      className={["mvx-switch", checked ? "mvx-switch--checked" : ""].filter(Boolean).join(" ")}
      onClick={() => onChange(!checked)}
    >
      <span className="mvx-switch__thumb" />
    </button>
  );
}

export type { CheckboxProps, SwitchProps, TextareaProps };
