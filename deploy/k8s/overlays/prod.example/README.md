# Production overlay (example)

> **Classification:** Current — The template a Kubernetes deployment starts from; docs/SELF_HOSTING.md walks through it.

A production overlay describes one cluster: its host names, storage class,
registry, mail relay and secrets. This directory is a complete one with
placeholder values, verified by deploying it. Copy it and fill it in:

```bash
cp -r deploy/k8s/overlays/prod.example deploy/k8s/overlays/prod
```

`deploy/k8s/overlays/prod/` is ignored by git in this repository, so a working
copy beside the base manifests can never be committed by accident; keep a
copy in a private repository of your own. The whole procedure — images,
secrets, the first deploy, sign-in setup, backups — is in
[docs/SELF_HOSTING.md](../../../../docs/SELF_HOSTING.md#install-on-kubernetes).

## What is here

| File | What it does |
|---|---|
| `kustomization.yaml` | Base plus everything below; replicas; pull-always patches; the `images:` block that points base's image names at your registry (paste the one `scripts/build-images.sh` prints) |
| `site.yaml` | Every value that belongs to your site — each marked `CHANGE`: host names, storage class, SMTP relay, alert address; Keycloak on PostgreSQL behind its public name; nightly dumps that include Keycloak's database |
| `networkpolicy-keycloak.yaml` | Keycloak's egress to PostgreSQL and the mail relay, which base (in-memory Keycloak) does not need |
| `pdb-gateway.yaml` | Keeps a gateway replica through node drains |
| `minio-remove.yaml`, `networkpolicy-external-objectstore.yaml` | Listed only when object storage lives outside the cluster — where backups belong |

## Secrets

The overlay ships no Secret, deliberately: pods that need one stay in
`CreateContainerConfigError` until you create it, which beats starting with a
published signing key. Create them with `kubectl create secret` (the manual's
step 4), or seal them for git with `scripts/seal-secrets.sh`,
`scripts/seal-integration-secret.sh` and `scripts/seal-smtp-secret.sh` and
list the results under `resources:`. Sealed output is encrypted to one
cluster's key and is useless in any other, so it is safe to commit — to your
private repository.
