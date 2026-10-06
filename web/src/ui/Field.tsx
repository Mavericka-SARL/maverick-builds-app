import React, { useId } from "react";

interface FieldProps {
  label: string;
  required?: boolean;
  description?: string;
  error?: string;
  children: React.ReactNode;
  style?: React.CSSProperties;
}

/**
 * A labelled form control.
 *
 * The label is associated with its control by id. It previously rendered as a
 * bare sibling <label> with no htmlFor, which looks identical but is not a
 * label at all as far as the accessibility tree is concerned: screen readers
 * announced the control unlabelled, clicking the text did not focus it, and
 * getByLabelText could not find it in tests. Across 93 uses that was every
 * form in the product.
 *
 * The id is injected into a single element child that does not already carry
 * one, so a control with its own id or aria-label keeps it and nothing that
 * renders several children is touched.
 *
 * The description and the error are tied to that child the same way
 * (aria-describedby, and aria-invalid while there is an error), whether or
 * not it brought its own id: a screen reader used to read neither the hint
 * nor the validation message of any form built on Field.
 */
type ControlProps = { id?: string; "aria-describedby"?: string; "aria-invalid"?: boolean };

export function Field({ label, required, description, error, children, style }: FieldProps) {
  const generatedId = useId();
  const only = React.Children.count(children) === 1 ? React.Children.only(children) : null;
  const control = React.isValidElement<ControlProps>(only) ? only : null;
  const target = control && !control.props.id ? control : null;
  const controlId = target ? generatedId : undefined;
  const descriptionId = `${generatedId}-description`;
  const errorId = `${generatedId}-error`;
  const describedBy = [control?.props["aria-describedby"], error ? errorId : description ? descriptionId : undefined]
    .filter(Boolean).join(" ") || undefined;
  const controlled = control
    ? React.cloneElement(control, {
        ...(target ? { id: controlId } : {}),
        "aria-describedby": describedBy,
        ...(error ? { "aria-invalid": true } : {}),
      })
    : children;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4, ...style }}>
      <label
        htmlFor={controlId}
        style={{ fontSize: 12, fontWeight: 600, color: "var(--color-text)", display: "flex", gap: 4 }}
      >
        {label}
        {required && <span style={{ color: "var(--color-danger)" }}>*</span>}
      </label>
      {controlled}
      {description && !error && (
        <span id={descriptionId} style={{ fontSize: 11, color: "var(--color-text-muted)" }}>{description}</span>
      )}
      {error && (
        <span id={errorId} style={{ fontSize: 11, color: "var(--color-danger)" }}>{error}</span>
      )}
    </div>
  );
}
