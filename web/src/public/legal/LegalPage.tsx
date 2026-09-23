import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../../api/client";
import { useBrand } from "../../branding/brand";
import { PublicPage } from "../PublicPage";
import { planAllowanceProse } from "../planAllowance";
import { privacyDocument, termsDocument, type PlanFacts } from "./documents";
import "../public.css";

/**
 * /terms and /privacy — the two documents a visitor is entitled to read before
 * they hand over a name and an e-mail address. Rendered outside the auth layer
 * (main.tsx), because requiring an account to read the terms of the account
 * would be absurd.
 *
 * Three states, all of them honest:
 *
 *   - the deployment publishes its own documents elsewhere, and this page
 *     sends the reader there;
 *   - it publishes the shipped documents, and this page renders them with the
 *     operator's identity filled in;
 *   - it publishes nothing, and this page says so plainly rather than showing
 *     a policy signed by nobody.
 */
export function LegalPage({ doc }: { doc: "terms" | "privacy" }) {
  const brand = useBrand();
  const { data: legal, isLoading } = useQuery({ queryKey: ["legal"], queryFn: api.legal });
  // The terms quote the plan the server actually offers, so its limits
  // cannot drift from what /signup promises.
  const { data: opts } = useQuery({ queryKey: ["signup-options"], queryFn: api.signupOptions, enabled: doc === "terms" });

  const external = legal ? (doc === "terms" ? legal.terms_url : legal.privacy_url) : "";
  const isExternal = /^https?:\/\//i.test(external);

  useEffect(() => {
    if (isExternal) window.location.replace(external);
  }, [isExternal, external]);

  if (isLoading || !legal) {
    return (
      <PublicPage testId={`legal-${doc}`}>
        <p>Loading…</p>
      </PublicPage>
    );
  }

  if (isExternal) {
    return (
      <PublicPage testId={`legal-${doc}`}>
        <h1>{doc === "terms" ? "Terms of service" : "Privacy notice"}</h1>
        <p>
          This document is published at <a href={external}>{external}</a>.
        </p>
      </PublicPage>
    );
  }

  if (!legal.builtin) {
    return (
      <PublicPage testId={`legal-${doc}`}>
        <h1>Not published</h1>
        <p data-testid="legal-unpublished">
          This deployment of {brand.name} has not published {doc === "terms" ? "terms of service" : "a privacy notice"}. Ask whoever
          operates it before you put anything into it.
        </p>
        <p className="mvx-public__footer">
          <a href="/">Back to sign in</a>
        </p>
      </PublicPage>
    );
  }

  const plan: PlanFacts | undefined =
    opts?.plan ? { name: opts.plan.name, allowance: planAllowanceProse(opts.plan.limits), next: opts.plan.limit_note || undefined } : undefined;
  const document = doc === "terms" ? termsDocument(legal, brand.name, plan) : privacyDocument(legal, brand.name);
  const other = doc === "terms" ? { href: legal.privacy_url, label: "Privacy notice" } : { href: legal.terms_url, label: "Terms of service" };

  return (
    <PublicPage testId={`legal-${doc}`} width="wide">
      <article className="mvx-public__doc">
        <h1>{document.title}</h1>
        <p className="mvx-public__doc-intro">{document.intro}</p>
        {legal.updated && (
          <p className="mvx-public__doc-date">
            In force from <time dateTime={legal.updated}>{formatDate(legal.updated)}</time>
          </p>
        )}

        {document.sections.map((section) => (
          <section key={section.heading}>
            <h2>{section.heading}</h2>
            {section.body.map((block, i) =>
              typeof block === "string" ? (
                <p key={i}>{block}</p>
              ) : (
                <ul key={i}>
                  {block.map((item) => (
                    <li key={item}>{item}</li>
                  ))}
                </ul>
              )
            )}
          </section>
        ))}

        <p className="mvx-public__footer">
          {other.href && (
            <>
              <a href={other.href}>{other.label}</a>
              {" · "}
            </>
          )}
          <a href="/signup">Create a workspace</a>
          {" · "}
          <a href="/">Sign in</a>
        </p>
      </article>
    </PublicPage>
  );
}

/** "2026-09-18" as "18 September 2026", and unchanged if it is not a date. */
function formatDate(iso: string): string {
  const d = new Date(`${iso}T00:00:00Z`);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString("en-GB", { day: "numeric", month: "long", year: "numeric", timeZone: "UTC" });
}
