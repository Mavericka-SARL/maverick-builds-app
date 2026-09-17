import { Button } from "../ui";
import type { StepCondition } from "../api/client";

/**
 * The decision controls for one pending workflow step, by step type — the
 * same rules in the business user's inbox and the business admin's task
 * list and history:
 *  - approval: Approve / Reject (the engine routes on "approve"/"reject");
 *  - task: one completion button (the developer's completion_label);
 *  - condition: reaches a human only when the engine could not evaluate it
 *    (its input is missing from the context) — the human picks the branch,
 *    since the engine routes a condition on "true"/"false" and any other
 *    decision routes nowhere (a generic "Complete" used to close the
 *    instance silently);
 *  - notification / join: automatic, nothing for a person to do.
 */
export function WorkflowStepActions({ stepType, completionLabel, condition, disabled, onDecide, size }: {
  stepType: string;
  completionLabel?: string;
  condition?: StepCondition | null;
  disabled?: boolean;
  onDecide: (decision: string) => void;
  size?: "sm";
}) {
  if (stepType === "approval") {
    return (
      <div style={{ display: "flex", gap: 8 }}>
        <Button variant="primary" size={size} style={{ flex: 1 }} disabled={disabled} onClick={() => onDecide("approve")}>Approve</Button>
        <Button variant="dangerSecondary" size={size} style={{ flex: 1 }} disabled={disabled} onClick={() => onDecide("reject")}>Reject</Button>
      </div>
    );
  }
  if (stepType === "condition") {
    return (
      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        <div className="mvx-admin-muted" style={{ fontSize: 13 }}>
          This condition could not be evaluated automatically
          {condition?.left ? <> — the request has no value for <code>{condition.left}</code></> : null}
          {condition ? <> (rule: <code>{condition.left} {condition.operator} {String(condition.right)}</code>)</> : null}. Choose the branch to continue.
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          <Button variant="primary" size={size} style={{ flex: 1 }} disabled={disabled} onClick={() => onDecide("true")}>Continue as true</Button>
          <Button variant="secondary" size={size} style={{ flex: 1 }} disabled={disabled} onClick={() => onDecide("false")}>Continue as false</Button>
        </div>
      </div>
    );
  }
  if (stepType === "notification" || stepType === "join") {
    return <div className="mvx-admin-muted" style={{ fontSize: 13 }}>Automatic step — the engine advances it, nothing to decide.</div>;
  }
  return (
    <div style={{ display: "flex", gap: 8 }}>
      <Button variant="primary" size={size} disabled={disabled} onClick={() => onDecide("complete")}>{completionLabel || "Complete"}</Button>
    </div>
  );
}
