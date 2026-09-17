# Production overlay (example)

> **Classification:** Current — the shape of a production overlay; the real one belongs elsewhere.

A production overlay describes one cluster: its storage class, its host names,
its image tags and its secrets. None of that belongs in this repository, so
this directory shows the shape and leaves the specifics to you.

Copy it, fill it in, and keep the result in a private repository of its own:

```bash
cp -r deploy/k8s/overlays/prod.example /path/to/your-private-deploy/prod
# edit kustomization.yaml, add your own patches, seal your secrets
kubectl apply -k /path/to/your-private-deploy/prod
```

`deploy/k8s/overlays/prod/` is ignored by git here, so a working copy kept
beside the base manifests can never be committed by accident.

## Secrets

The overlay ships no Secret, deliberately. `mavericks-secrets` once lived in
`base/`, which meant every overlay inherited `JWT_SECRET: changeme` and a
base64-encoded database password — encoding, not encryption, and anyone with
the repository had the credentials.

Seal your own against your cluster with `scripts/seal-secrets.sh`. Until you
do, pods that need the secret stay in `CreateContainerConfigError`. That
failure is the intended behaviour: the alternative was starting successfully
with a published signing key.

Sealed output is encrypted to one cluster's public key and is useless in any
other, so it is safe to commit — to your private deployment repository.

## What a real overlay adds

| Concern | What to put in your overlay |
|---|---|
| Storage | `storageClassName` for your provider |
| Host names | Ingress rules and the console/Keycloak origins |
| Images | Pin every image to a digest or a git-sha tag from CI, never `latest` |
| Secrets | A sealed secret, or an External Secrets / Vault reference |
| Availability | Replica counts, and a PodDisruptionBudget for the gateway |
| Transport | gRPC mTLS between services (cert-manager issues the shared cert) |
| Demo data | Remove the seed Job |
