# Staging overlay (example)

> **Classification:** Current — How a staging environment is set up next to production, and how it is used.

Staging runs what production runs — the same images and manifests, a real
identity provider, TLS at the ingress — in its own namespace on the same
cluster, on data nobody minds losing. A release candidate goes here first,
`cmd/loadtest` is pointed at it, and only then does the image tag move to the
production overlay.

Copy this directory into the private deployment repository next to the
production overlay, replace every `example.com` hostname, and keep it there:
like production, a staging overlay names a real cluster.

## What differs from production

| | Production | Staging |
|---|---|---|
| Namespace | `mavericks` | `mavericks-staging` |
| Hostnames | `console`, `auth.` | `staging.`, `auth-staging.` |
| Replicas | gateway 2, web 2 | one of everything |
| Secrets | sealed (`scripts/seal-secrets.sh`) | created on the cluster by hand (below) |
| Sign-up | as configured | `SIGNUP_ENABLED: "true"` |
| Database volume | 20Gi | 10Gi |
| Object storage | `mavericks` bucket | `mavericks-staging`, backups in `mavericks-backups-staging`, WAL under `staging/` |

Everything else — mTLS between services, Keycloak on Postgres, the ingress
annotations, the network policies — is the production shape; copy the
production overlay's `mtls.yaml` and network-policy patches with the
namespace changed.

## Bringing it up

1. **DNS.** A records for both hostnames pointing at the ingress-nginx load
   balancer. Certificates cannot be issued before this resolves.
2. **Secrets, by hand.** Staging's secrets are throwaway values, so they are
   not worth sealing; the object-storage key is the one value shared with
   production. On the cluster:

   ```bash
   kubectl create namespace mavericks-staging
   # image pull credentials: the same secret production uses, on the default service account
   kubectl -n mavericks get secret ghcr-pull -o yaml | sed 's/namespace: mavericks$/namespace: mavericks-staging/' | kubectl apply -f -
   kubectl -n mavericks-staging patch serviceaccount default -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
   kubectl -n mavericks-staging create secret generic mavericks-secrets \
     --from-literal=POSTGRES_PASSWORD="$(openssl rand -base64 24)" \
     --from-literal=JWT_SECRET="$(openssl rand -base64 48)" \
     --from-literal=KEYCLOAK_ADMIN_PASSWORD="$(openssl rand -base64 24)" \
     --from-literal=MINIO_ROOT_PASSWORD="<the object-storage secret key>" \
     --from-literal=KEYCLOAK_ADMIN_CLIENT_SECRET="pending"
   kubectl -n mavericks-staging create secret generic mavericks-integration \
     --from-literal=INTEGRATION_CRED_KEY="$(openssl rand -base64 32)"
   ```

3. **Apply**, then create Keycloak's database, exactly as in the production
   guide:

   ```bash
   kubectl apply -k <private repo>/staging
   kubectl -n mavericks-staging exec postgres-0 -c postgres -- \
     psql -U mavericks -d mavericks -c 'CREATE DATABASE keycloak OWNER mavericks'
   kubectl -n mavericks-staging rollout restart deployment/keycloak
   ```

4. **Provisioning service account**, so the console and sign-up can create
   accounts: `scripts/keycloak-provisioning-setup.sh` against the staging
   Keycloak, then put the secret it prints into `mavericks-secrets` as
   `KEYCLOAK_ADMIN_CLIENT_SECRET` and restart the gateway.
5. **First administrator**: `scripts/bootstrap-platform-admin.sh` creates
   the Keycloak account and the platform_admin row in one go. From then on
   the console creates everyone else.
6. **SMTP** on the realm (`scripts/keycloak-smtp-setup.sh`), or invitations
   — and sign-ups — cannot be completed.

## Using it

- Deploy a candidate by changing `images:` and applying.
- `go run ./cmd/loadtest -base https://staging.example.com -keycloak
  https://auth-staging.example.com -username <account> -password <pw>
  -users 100 -duration 2m -target-p95 2s` drives the account's application;
  `-json` keeps the report. Before DNS exists, add `-resolve host=ip` for both
  hostnames and `-insecure`.
- Sign up at `https://staging.example.com/signup` to see the trial funnel end
  to end.
