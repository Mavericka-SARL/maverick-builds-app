import { useEffect, useState, type ReactNode } from "react";
import keycloak from "./keycloak";
import { api } from "../api/client";
import { AuthContext } from "./context";
import { config } from "../config";
import { useBrand } from "../branding/brand";
import { PublicPage } from "../public/PublicPage";
import { LegalFooter } from "../public/LegalFooter";

const DEV_MODE = config.devMode;

const DEV_PERSONAS: Record<string, { email: string; name: string; roles: string[] }> = {
  // OPEX Planning personas
  dept_head:       { email: "dept.head@acme.com",          name: "Alex (Dept Head)",        roles: ["business_user"] },
  finance:         { email: "finance@acme.com",            name: "Jordan (Finance)",        roles: ["business_admin"] },
  developer:       { email: "dev@acme.com",                name: "Sam (Developer)",         roles: ["developer"] },
  tenant_admin:    { email: "admin@acme.com",              name: "Pat (Tenant Admin)",      roles: ["tenant_admin"] },
  platform_admin:  { email: "platform-admin@acme.com",     name: "Pat (Platform Admin)",    roles: ["platform_admin"] },
  // Budget Planning personas
  cfo:             { email: "cfo@acme.com",                name: "Dana (CFO)",              roles: ["business_admin"] },
  finance_mgr:     { email: "finance.manager@acme.com",    name: "Riley (Finance Mgr)",     roles: ["business_admin"] },
  dept_eng:        { email: "dept.head.eng@acme.com",      name: "Alex (Eng Head)",         roles: ["business_user"] },
  dept_sales:      { email: "dept.head.sales@acme.com",    name: "Morgan (Sales Head)",     roles: ["business_user"] },
  dept_ops:        { email: "dept.head.ops@acme.com",      name: "Jordan (Ops Head)",       roles: ["business_user"] },
};

function DevAuthProvider({ children }: { children: ReactNode }) {
  const [persona, setPersonaState] = useState<string>(
    () => localStorage.getItem("dev_persona") ?? "dept_head"
  );
  const [roles, setRoles] = useState<string[]>(() => {
    const initial = localStorage.getItem("dev_persona") ?? "dept_head";
    return (DEV_PERSONAS[initial] ?? DEV_PERSONAS.dept_head).roles;
  });

  function setPersona(p: string) {
    setRoles((DEV_PERSONAS[p] ?? DEV_PERSONAS.dept_head).roles);
    localStorage.setItem("dev_persona", p);
    // A different persona can belong to an entirely different tenant/app —
    // carrying over the previous persona's selected_app_id makes apiFetch
    // send an X-App-Id the new persona has no access to. /api/demo's own
    // "resolve model" error self-heals from this (apiFetch retries once
    // without the stale header), but errors that don't match that retry's
    // regex — e.g. dashboard-widgets' "dashboard not accessible" — don't,
    // and surface as a permanently broken chart/widget instead.
    localStorage.removeItem("selected_app_id");
    setPersonaState(p);
  }

  useEffect(() => {
    let cancelled = false;
    const fallback = (DEV_PERSONAS[persona] ?? DEV_PERSONAS.dept_head).roles;

    // /api/me resolves the actor's roles with no model/app context — unlike
    // /api/demo, it works for users whose tenant has no applications yet.
    fetch("/api/me", {
      headers: {
        "Content-Type": "application/json",
        "X-Dev-User": persona,
      },
    })
      .then((res) => {
        if (!res.ok) throw new Error(res.statusText);
        return res.json() as Promise<{ roles?: string[] }>;
      })
      .then((data) => {
        if (!cancelled && Array.isArray(data.roles)) {
          setRoles([...new Set(data.roles)]);
        }
      })
      .catch(() => {
        if (!cancelled) setRoles(fallback);
      });

    return () => {
      cancelled = true;
    };
  }, [persona]);

  return (
    <AuthContext.Provider
      value={{
        authenticated: true,
        token: undefined,
        userRoles: roles,
        persona,
        setPersona,
        logout: () => setPersona("dept_head"),
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}

/**
 * Before anyone is signed in: whether this deployment has single sign-on at
 * all, and — given an address — which provider to send the person to. The
 * endpoint is public and answers only what a sign-in page must know.
 */
async function discoverSso(email?: string): Promise<{ sso: boolean; alias?: string; display_name?: string }> {
  const q = email ? `?email=${encodeURIComponent(email)}` : "";
  const res = await fetch(`/api/sso/discover${q}`);
  if (!res.ok) return { sso: false };
  return res.json();
}

/**
 * Shown only on deployments where some tenant has single sign-on. Everyone
 * else never sees it: the console goes straight to the realm login as it
 * always did. A person types their work address; if the domain belongs to a
 * tenant with a provider they are sent there, otherwise to the usual login.
 */
function SignIn({ onPassword }: { onPassword: () => void }) {
  const brand = useBrand();
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const company = async () => {
    setBusy(true);
    const d = await discoverSso(email.trim());
    setBusy(false);
    if (d.alias) {
      await keycloak.login({ idpHint: d.alias, loginHint: email.trim() });
      return;
    }
    setNote("No company sign-in is registered for that address. Sign in with your password instead.");
  };
  return (
    <PublicPage testId="sign-in">
      {/* The mark above the form already says whose sign-in this is; only a
          white-labelled name, which is text and not a mark, is worth
          repeating in the heading. */}
      <h1>{brand.configured && brand.product_name ? `Sign in to ${brand.product_name}` : "Sign in"}</h1>
      <p>{brand.configured && brand.tagline ? brand.tagline : "Use your company account, or your password."}</p>
      <form className="mvx-public__form" onSubmit={(e) => { e.preventDefault(); void company(); }}>
        <input className="mvx-public__field" type="email" value={email} onChange={(e) => setEmail(e.target.value)}
          placeholder="you@company.com" aria-label="Work e-mail" autoComplete="email" />
        <button className="mvx-public__button" type="submit" disabled={busy || !email.includes("@")}>
          {busy ? "Looking up…" : "Continue with company account"}
        </button>
        <button className="mvx-public__button mvx-public__button--ghost" type="button" onClick={onPassword}>Sign in with password</button>
      </form>
      {note && <p role="status" className="mvx-public__note">{note}</p>}
      <LegalFooter />
    </PublicPage>
  );
}

function ProdAuthProvider({ children }: { children: ReactNode }) {
  const [authenticated, setAuthenticated] = useState(false);
  const [token, setToken] = useState<string | undefined>();
  const [userRoles, setUserRoles] = useState<string[]>([]);
  const [needsSignIn, setNeedsSignIn] = useState(false);
  const [refused, setRefused] = useState<string | null>(null);

  useEffect(() => {
    // check-sso instead of login-required: a deployment with single sign-on
    // needs a page BEFORE the realm login to route people to their own
    // provider. Without any provider registered the behaviour is unchanged —
    // straight to the realm login.
    keycloak
      .init({ onLoad: "check-sso", pkceMethod: "S256" })
      .then(async (auth) => {
        if (!auth) {
          const d = await discoverSso();
          if (d.sso) setNeedsSignIn(true);
          else await keycloak.login();
          return;
        }
        setAuthenticated(auth);
        setToken(keycloak.token);
        // The console is composed from the roles the SERVER authorizes with
        // (identity.role_assignment, what /api/me returns and what the Users
        // panel grants) — not from the token's realm roles. Those are set
        // once at Keycloak user creation and never follow later grants: a
        // production tenant_admin granted through the Users panel carried
        // only business_admin in the token, so the Tenant admin group (model
        // export/import) never appeared while the account badge, which reads
        // /api/me, showed the role (reported live, 2026-09-10). Realm roles
        // are only the fallback until /api/me answers, or if it cannot.
        setUserRoles(keycloak.realmAccess?.roles ?? []);
        api.getMe()
          .then((me) => {
            if (Array.isArray(me.roles) && me.roles.length > 0) setUserRoles([...new Set(me.roles)]);
          })
          .catch((err: Error) => {
            // A first single sign-on login the tenant does not allow answers
            // 403 with the reason; show it rather than an empty console.
            if (/403|not|forbidden/i.test(err.message)) setRefused(err.message);
          });

        setInterval(() => {
          keycloak.updateToken(60).then((refreshed) => {
            if (refreshed) setToken(keycloak.token);
          });
        }, 30_000);
      });
  }, []);

  if (needsSignIn && !authenticated) return <SignIn onPassword={() => void keycloak.login()} />;
  if (!authenticated) return null;
  if (refused) {
    return (
      <PublicPage testId="sign-in-refused">
        <h1>Your account could not be created</h1>
        <p>{refused.replace(/^\w+ \d+: ?/, "")}</p>
        <button className="mvx-public__button mvx-public__button--ghost" type="button" onClick={() => keycloak.logout()}>Sign out</button>
      </PublicPage>
    );
  }

  return (
    <AuthContext.Provider
      value={{
        authenticated,
        token,
        userRoles,
        persona: "prod",
        setPersona: () => {},
        logout: () => keycloak.logout(),
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}

export function AuthProvider({ children }: { children: ReactNode }) {
  return DEV_MODE
    ? <DevAuthProvider>{children}</DevAuthProvider>
    : <ProdAuthProvider>{children}</ProdAuthProvider>;
}
