import type { ReactNode } from "react";
import { BrandMark } from "../branding/BrandMark";
import "./public.css";

/**
 * The frame every pre-account page shares: the dark ground, the brand mark,
 * and one column of content — narrow for a form, wide enough to read prose in
 * for the terms and the privacy notice. A deployment that has configured
 * white-labelling gets its own logo here; everyone else gets the product's
 * own wordmark.
 */
export function PublicPage({ children, testId, width = "narrow" }: { children: ReactNode; testId?: string; width?: "narrow" | "wide" }) {
  return (
    <div className="mvx-public" data-testid={testId}>
      <div className={width === "wide" ? "mvx-public__inner mvx-public__inner--wide" : "mvx-public__inner"}>
        <BrandMark markClassName="mvx-public__mark" logoClassName="mvx-public__logo" />
        {children}
      </div>
    </div>
  );
}
