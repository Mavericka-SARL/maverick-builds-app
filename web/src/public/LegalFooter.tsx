import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { externalAttrs } from "./legalLinks";

/**
 * The quiet line of legal links at the foot of a front-door page. It renders
 * nothing at all when the deployment publishes no documents, so a self-hosted
 * install never shows a dead link to a policy that does not exist.
 */
export function LegalFooter() {
  const { data: legal } = useQuery({ queryKey: ["legal"], queryFn: api.legal });
  if (!legal?.published) return null;
  return (
    <p className="mvx-public__footer" data-testid="legal-footer">
      <a href={legal.terms_url} {...externalAttrs(legal.terms_url)}>
        Terms of service
      </a>
      {" · "}
      <a href={legal.privacy_url} {...externalAttrs(legal.privacy_url)}>
        Privacy notice
      </a>
    </p>
  );
}
