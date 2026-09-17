import { useMemo, useState } from "react";
import { useAuth } from "../auth/useAuth";
import { useBrand, useBrandRefresh } from "../branding/brand";
import { AppShell, NotificationCenter } from "../ui";
import { PersonaSwitcher } from "../shell/PersonaSwitcher";
import { PlanBanner } from "../shell/PlanBanner";
import { UserMenu } from "../shell/UserMenu";
import { useBusinessSection } from "../consoles/business/BusinessConsole";
import { useBusinessAdminSection } from "../consoles/business-admin/section";
import { useDeveloperSection } from "../consoles/developer/DeveloperConsole";
import { useAdminSection } from "../consoles/platform-admin/section";
import { consoleSubtitle, enabledSections, resolveTab, sectionOf, type ConsoleSection, type SectionId } from "./sections";

/**
 * The one console. Every role the signed-in user holds contributes its
 * sidebar group(s) and screens (see router/sections.ts for the rule and the
 * order); holding several roles adds groups to this sidebar, it never opens
 * a second console. The section hooks all run on every render — React needs
 * a stable hook order — and each returns null, having fetched nothing, when
 * the user's roles do not grant it.
 */
export default function UnifiedConsole() {
  const { userRoles, persona } = useAuth();
  const brand = useBrand();
  // The brand by tenant, now that the caller is known (and again when the
  // dev persona changes tenant).
  useBrandRefresh(persona);
  const ids = useMemo(() => enabledSections(userRoles), [userRoles]);
  const [chosen, setTab] = useState<string>("");
  // A remembered choice from a section the user (no longer) holds falls
  // back to the landing tab of their first section.
  const tab = resolveTab(chosen, ids);
  const input = (id: SectionId) => ({ enabled: ids.includes(id), tab, setTab, roles: userRoles });

  const candidates: (ConsoleSection | null)[] = [
    useBusinessSection(input("business")),
    useBusinessAdminSection(input("business-admin")),
    useDeveloperSection(input("developer")),
    useAdminSection({ ...input("tenant-admin"), scope: "tenant" }),
    useAdminSection({ ...input("platform-admin"), scope: "platform" }),
  ];
  const sections = candidates.filter((s): s is ConsoleSection => s !== null);
  const owner = sections.find((s) => s.id === sectionOf(tab));

  return (
    <AppShell
      productName={brand.configured && brand.product_name ? brand.product_name : undefined}
      logo={brand.configured && brand.logo_data_url ? <img src={brand.logo_data_url} alt={brand.name} data-testid="brand-logo" /> : undefined}
      productSubtitle={consoleSubtitle(ids)}
      navGroups={sections.flatMap((s) => s.navGroups)}
      activeNavId={tab}
      onNavSelect={(item) => setTab(item.id)}
      utility={
        <>
          <NotificationCenter
            onNavigate={(resourceType, resourceId) => {
              for (const s of sections) {
                if (s.onNotificationNavigate?.(resourceType, resourceId)) return;
              }
            }}
          />
          <UserMenu />
        </>
      }
      contextItems={owner?.contextItems ?? []}
      sidebarFooter={<PersonaSwitcher />}
    >
      <PlanBanner />
      {owner?.render(tab)}
    </AppShell>
  );
}
