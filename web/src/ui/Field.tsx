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
 */
export function Field({ label, required, description, error, children, style }: FieldProps) {
  const generatedId = useId();
  const only = React.Children.count(children) === 1 ? React.Children.only(children) : null;
  const target = React.isValidElement<{ id?: string }>(only) && !only.props.id ? only : null;
  const controlId = target ? generatedId : undefined;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4, ...style }}>
      <label
        htmlFor={controlId}
        style={{ fontSize: 12, fontWeight: 600, color: "var(--color-text)", display: "flex", gap: 4 }}
      >
        {label}
        {required && <span style={{ color: "var(--color-danger)" }}>*</span>}
      </label>
      {target ? React.cloneElement(target, { id: controlId }) : children}
      {description && !error && (
        <span style={{ fontSize: 11, color: "var(--color-text-muted)" }}>{description}</span>
      )}
      {error && (
        <span style={{ fontSize: 11, color: "var(--color-danger)" }}>{error}</span>
      )}
    </div>
  );
}
