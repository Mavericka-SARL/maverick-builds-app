import React from "react";

interface Tab<T extends string> {
  id: T;
  label: string;
  count?: number;
}

interface TabsProps<T extends string> {
  tabs: Tab<T>[];
  active: T;
  onChange: (id: T) => void;
  style?: React.CSSProperties;
}

export function Tabs<T extends string>({ tabs, active, onChange, style }: TabsProps<T>) {
  return (
    <div className="mvx-tabs" style={style}>
      {tabs.map((tab) => {
        const isActive = tab.id === active;
        return (
          <button
            key={tab.id}
            onClick={() => onChange(tab.id)}
            className={["mvx-tabs__button", isActive ? "mvx-tabs__button--active" : ""].filter(Boolean).join(" ")}
            aria-pressed={isActive}
          >
            {tab.label}
            {tab.count !== undefined && (
              <span className={["mvx-tabs__count", isActive ? "mvx-tabs__count--active" : ""].filter(Boolean).join(" ")}>
                {tab.count}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
