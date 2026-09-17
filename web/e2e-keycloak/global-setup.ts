import { Client } from "pg";

const KEYCLOAK_URL = process.env.VITE_KEYCLOAK_URL ?? "http://localhost:8180";
const REALM = process.env.VITE_KEYCLOAK_REALM ?? "mavericks";
const DATABASE_URL =
  process.env.DATABASE_URL ?? "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable";

// The seeded realm users (deploy/docker/config/keycloak/mavericks-realm.json)
// have no pinned "id", so Keycloak assigns each a real UUID sub on import —
// one that has no corresponding internal/identity.user row (the dev-mode
// seed script keys that table by hand-picked strings like
// "demo-dept-head-001" for its own, separate X-Dev-User persona path). A
// real login as "alex" would otherwise authenticate fine and then fail
// every API call with "actor not found". This setup step looks up alex's
// real Keycloak-issued id and upserts a matching identity.user row so the
// test proves the actual P0-1 wiring (real Bearer token → resolveJWTActor
// → a resolvable actor), without touching the shared seed/realm files that
// the dev-mode flow and other tests still depend on.
export default async function globalSetup() {
  const tokenRes = await fetch(`${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "password",
      client_id: "admin-cli",
      username: "admin",
      password: "admin",
    }),
  });
  if (!tokenRes.ok) {
    throw new Error(`keycloak admin login failed: ${tokenRes.status} ${await tokenRes.text()}`);
  }
  const { access_token: adminToken } = (await tokenRes.json()) as { access_token: string };

  const usersRes = await fetch(
    `${KEYCLOAK_URL}/admin/realms/${REALM}/users?username=alex&exact=true`,
    { headers: { Authorization: `Bearer ${adminToken}` } },
  );
  if (!usersRes.ok) {
    throw new Error(`keycloak user lookup failed: ${usersRes.status} ${await usersRes.text()}`);
  }
  const users = (await usersRes.json()) as Array<{ id: string; email: string }>;
  if (users.length === 0) {
    throw new Error("keycloak user 'alex' not found in realm " + REALM);
  }
  const alexSub = users[0].id;

  const client = new Client({ connectionString: DATABASE_URL });
  await client.connect();
  try {
    // Keyed on email, not keycloak_sub: a stale row from a prior dev-mode
    // cmd/seed run would already own this same email under the hand-picked
    // "demo-dept-head-001" sub — this repoints it at alex's real Keycloak
    // id instead of colliding on the email UNIQUE constraint.
    await client.query(
      `INSERT INTO identity.user (keycloak_sub, email, display_name)
       VALUES ($1, $2, 'Alex (E2E)')
       ON CONFLICT (email) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub, display_name = EXCLUDED.display_name`,
      [alexSub, users[0].email],
    );
  } finally {
    await client.end();
  }
}
