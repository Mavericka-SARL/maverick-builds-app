import { useAuth } from "../auth/useAuth";
import { AppShell, PageLayout, EmptyState } from "../ui";
import { PersonaSwitcher } from "../shell/PersonaSwitcher";
import UnifiedConsole from "./UnifiedConsole";
import { enabledSections } from "./sections";

/**
 * Renders the one console for any user with at least one role. Roles are
 * additive and each adds its tabs to that console (router/sections.ts); the
 * only routing decision left here is "any role at all?".
 */
export default function RoleRouter() {
  const { userRoles, logout } = useAuth();

  if (enabledSections(userRoles).length === 0) {
    return (
      <AppShell navGroups={[]} sidebarFooter={<PersonaSwitcher />}>
        <PageLayout title="No role assigned">
          <EmptyState
            label="Your account has no assigned role. Contact your administrator."
            action="Sign out"
            onAction={logout}
          />
        </PageLayout>
      </AppShell>
    );
  }

  return <UnifiedConsole />;
}
