interface Segment<T extends string> {
  id: T;
  label: string;
  disabled?: boolean;
}

interface SegmentedControlProps<T extends string> {
  segments: Segment<T>[];
  value: T;
  onChange: (value: T) => void;
  "aria-label"?: string;
  className?: string;
}

export function SegmentedControl<T extends string>({
  segments,
  value,
  onChange,
  "aria-label": ariaLabel,
  className,
}: SegmentedControlProps<T>) {
  return (
    <div className={["mvx-segmented", className].filter(Boolean).join(" ")} role="group" aria-label={ariaLabel}>
      {segments.map((segment) => {
        const active = segment.id === value;
        return (
          <button
            key={segment.id}
            type="button"
            className={["mvx-segmented__button", active ? "mvx-segmented__button--active" : ""].filter(Boolean).join(" ")}
            aria-pressed={active}
            disabled={segment.disabled}
            onClick={() => onChange(segment.id)}
          >
            {segment.label}
          </button>
        );
      })}
    </div>
  );
}

export type { Segment, SegmentedControlProps };
