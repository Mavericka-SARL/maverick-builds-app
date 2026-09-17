import { useQuery } from "@tanstack/react-query";
import { Select } from "../ui";
import { useAuth } from "../auth/useAuth";
import { api } from "../api/client";
import { config } from "../config";

const DEV_MODE = config.devMode;

// Fallback while /api/dev/personas is loading (or unreachable): the seeded
// demo personas. Once the server list arrives it replaces this entirely, so
// users created through the admin console show up automatically.
const FALLBACK_PERSONAS: Record<string, string> = {
  dept_head:      "Alex — Dept Head",
  finance:        "Jordan — Finance",
  developer:      "Sam — Developer",
  tenant_admin:   "Pat — Tenant Admin",
  platform_admin: "Pat — Platform Admin",
};

function personaLabel(label: string, roles: string[]): string {
  // Seeded users carry their role in the display name ("Alex (Dept Head)");
  // only annotate the ones that don't.
  if (roles.length === 0 || label.includes("(")) return label;
  return `${label} — ${roles.join(", ").replaceAll("_", " ")}`;
}

/**
 * Dev-only persona switcher rendered in the AppShell sidebar footer.
 * Lists every user in the database (admin-created ones included).
 * Switching personas resets the selected application and reloads so every
 * scoped query re-resolves against the new identity.
 */
export function PersonaSwitcher() {
  const { persona, setPersona } = useAuth();

  const { data: personas } = useQuery({
    queryKey: ["dev-personas"],
    queryFn: api.listDevPersonas,
    enabled: DEV_MODE,
    staleTime: 15_000,
  });

  if (!DEV_MODE) return null;

  const options: [string, string][] =
    personas && personas.length > 0
      ? personas.map((p) => [p.key, personaLabel(p.label, p.roles)])
      : Object.entries(FALLBACK_PERSONAS);
  // Keep the stored selection valid even if it's not in the list (e.g. the
  // persona's user was deleted) so the select doesn't silently jump.
  if (!options.some(([key]) => key === persona)) {
    options.push([persona, persona]);
  }

  return (
    <div className="mvx-persona-switcher">
      <div className="mvx-persona-switcher__label">Dev — Persona</div>
      <Select
        value={persona}
        aria-label="Switch persona"
        onChange={(e) => {
          setPersona(e.target.value);
          window.location.reload();
        }}
      >
        {options.map(([key, label]) => (
          <option key={key} value={key}>
            {label}
          </option>
        ))}
      </Select>
    </div>
  );
}
