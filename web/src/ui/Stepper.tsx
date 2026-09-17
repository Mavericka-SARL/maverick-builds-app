import React from "react";
import { Check } from "lucide-react";

interface StepperStep {
  id: string;
  label: string;
}

interface StepperProps {
  steps: StepperStep[];
  /** id of the current step */
  current: string;
  className?: string;
}

export function Stepper({ steps, current, className }: StepperProps) {
  const idx = steps.findIndex(s => s.id === current);
  return (
    <div className={["mvx-stepper", className].filter(Boolean).join(" ")}>
      {steps.map((step, i) => (
        <React.Fragment key={step.id}>
          <div className="mvx-stepper__step" aria-current={i === idx ? "step" : undefined}>
            <div className={["mvx-stepper__dot", i <= idx ? "mvx-stepper__dot--done" : ""].filter(Boolean).join(" ")}>
              {i < idx ? <Check size={13} aria-hidden="true" /> : i + 1}
            </div>
            <span className={[
              "mvx-stepper__label",
              i === idx ? "mvx-stepper__label--current" : "",
              i <= idx ? "mvx-stepper__label--reached" : "",
            ].filter(Boolean).join(" ")}>
              {step.label}
            </span>
          </div>
          {i < steps.length - 1 && (
            <div className={["mvx-stepper__bar", i < idx ? "mvx-stepper__bar--done" : ""].filter(Boolean).join(" ")} />
          )}
        </React.Fragment>
      ))}
    </div>
  );
}

export type { StepperProps, StepperStep };
