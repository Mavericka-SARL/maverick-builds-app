import type React from "react";

interface PropertyPanelProps {
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  footer?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}

export function PropertyPanel({ title, subtitle, actions, footer, children, className }: PropertyPanelProps) {
  return (
    <aside className={["mvx-property-panel", className].filter(Boolean).join(" ")}>
      <div className="mvx-property-panel__header">
        <div className="mvx-property-panel__heading">
          <div>
            <h3 className="mvx-property-panel__title">{title}</h3>
            {subtitle && <div className="mvx-section-header__subtitle">{subtitle}</div>}
          </div>
          {actions}
        </div>
      </div>
      <div className="mvx-property-panel__body">{children}</div>
      {footer && <div className="mvx-property-panel__footer">{footer}</div>}
    </aside>
  );
}

export type { PropertyPanelProps };
