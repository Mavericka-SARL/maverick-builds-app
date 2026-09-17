import React from "react";

interface PageHeaderProps {
  title: string;
  subtitle?: string;
  meta?: React.ReactNode;
  actions?: React.ReactNode;
}

export function PageHeader({ title, subtitle, meta, actions }: PageHeaderProps) {
  return (
    <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", gap: 16, marginBottom: 20 }}>
      <div>
        <h2 style={{ margin: 0, fontSize: 18, fontWeight: 700, color: "var(--color-text)", lineHeight: 1.25 }}>
          {title}
        </h2>
        {(subtitle || meta) && (
          <div style={{ marginTop: 4, fontSize: 12, color: "var(--color-text-muted)", display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            {subtitle && <span>{subtitle}</span>}
            {meta && <span>{meta}</span>}
          </div>
        )}
      </div>
      {actions && (
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexShrink: 0 }}>
          {actions}
        </div>
      )}
    </div>
  );
}
