import type React from "react";

type PageWidth = "full" | "wide" | "narrow";

interface PageLayoutProps {
  title?: React.ReactNode;
  subtitle?: React.ReactNode;
  meta?: React.ReactNode;
  actions?: React.ReactNode;
  width?: PageWidth;
  children: React.ReactNode;
  className?: string;
}

interface SectionHeaderProps {
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
}

export function PageLayout({
  title,
  subtitle,
  meta,
  actions,
  width = "full",
  children,
  className,
}: PageLayoutProps) {
  const widthClass = width === "full" ? "" : `mvx-page--${width}`;
  return (
    <main className={["mvx-page", widthClass, className].filter(Boolean).join(" ")}>
      {(title || subtitle || meta || actions) && (
        <header className="mvx-page__header">
          <div>
            {title && <h2 className="mvx-page__title">{title}</h2>}
            {(subtitle || meta) && (
              <div className="mvx-page__subtitle">
                {subtitle}
                {subtitle && meta ? " " : null}
                {meta}
              </div>
            )}
          </div>
          {actions && <div className="mvx-page__actions">{actions}</div>}
        </header>
      )}
      {children}
    </main>
  );
}

export function SectionHeader({ title, subtitle, actions, className }: SectionHeaderProps) {
  return (
    <div className={["mvx-section-header", className].filter(Boolean).join(" ")}>
      <div>
        <h3 className="mvx-section-header__title">{title}</h3>
        {subtitle && <div className="mvx-section-header__subtitle">{subtitle}</div>}
      </div>
      {actions && <div className="mvx-section-header__actions">{actions}</div>}
    </div>
  );
}

export type { PageLayoutProps, PageWidth, SectionHeaderProps };
