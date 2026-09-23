#!/usr/bin/env bash
# Seals the platform's own encryption keys for one cluster, as the
# `mavericks-integration` Secret the gateway (seals at write time) and the
# integration worker (opens at run time) both mount:
#
#   INTEGRATION_CRED_KEY    REST connector credentials, OAuth tokens and
#                           tenants' Google service accounts — mandatory,
#                           there is no plaintext fallback
#   SECRETS_ENCRYPTION_KEY  AI provider keys (internal/secretbox); without
#                           it they are stored in plaintext
#
# These two keys are the deployment's most consequential secrets: every
# credential a tenant ever stored is sealed under them, and a deployment
# rebuilt with fresh keys cannot open any of it. That is why they are sealed
# into the private overlay like the database password rather than created
# by hand on the cluster — a hand-made Secret exists nowhere but in the
# cluster, and `kubectl create secret … $(openssl rand …)` on a rebuild
# silently mints new keys. Separate from scripts/seal-secrets.sh so that
# rotating the database password never touches these.
#
# Everything scripts/seal-secrets.sh says about sealed-secrets applies: the
# output is bound to one cluster, is safe to commit, and belongs in that
# deployment's private repository.
#
# Usage — a NEW deployment generates the keys here, once:
#   INTEGRATION_CRED_KEY="$(openssl rand -base64 48)" \
#   SECRETS_ENCRYPTION_KEY="$(openssl rand -base64 48)" \
#   bash scripts/seal-integration-secret.sh
#
# An EXISTING deployment whose Secret was created by hand seals the values
# it already runs with — never new ones:
#   INTEGRATION_CRED_KEY="$(kubectl -n mavericks get secret mavericks-integration -o jsonpath='{.data.INTEGRATION_CRED_KEY}' | base64 -d)" \
#   SECRETS_ENCRYPTION_KEY="$(kubectl -n mavericks get secret mavericks-integration -o jsonpath='{.data.SECRETS_ENCRYPTION_KEY}' | base64 -d)" \
#   bash scripts/seal-integration-secret.sh
# and, before applying, lets the controller adopt the hand-made Secret
# (it refuses to overwrite one it does not own):
#   kubectl -n mavericks annotate secret mavericks-integration sealedsecrets.bitnami.com/managed=true
#
# Then add the output file to the overlay's kustomization.yaml and apply.
# The pods keep the values they were started with; nothing needs a restart
# when the values are unchanged.
set -euo pipefail

OUT="${OUT:-deploy/k8s/overlays/prod/sealed-integration.yaml}"
NS="mavericks"
NAME="mavericks-integration"

command -v kubeseal >/dev/null || { echo "kubeseal not found on PATH" >&2; exit 1; }
command -v kubectl  >/dev/null || { echo "kubectl not found on PATH" >&2; exit 1; }

ctx=$(kubectl config current-context)
echo "Sealing against context: ${ctx}"
read -r -p "Is that the cluster these keys are for? [y/N] " ok
[ "${ok:-n}" = "y" ] || { echo "aborted"; exit 1; }

# Read from the environment, never from arguments (visible in ps).
: "${INTEGRATION_CRED_KEY:?missing required env var: INTEGRATION_CRED_KEY}"
: "${SECRETS_ENCRYPTION_KEY:?missing required env var: SECRETS_ENCRYPTION_KEY}"

# A key that already seals data on this cluster must be the one sealed here:
# refuse to overwrite a live Secret with different values.
if live=$(kubectl -n "$NS" get secret "$NAME" -o jsonpath='{.data.INTEGRATION_CRED_KEY}' 2>/dev/null) && [ -n "$live" ]; then
  if [ "$(printf '%s' "$live" | base64 -d)" != "$INTEGRATION_CRED_KEY" ]; then
    echo "refusing: the cluster already runs with a different INTEGRATION_CRED_KEY — seal the live value (see the usage note)" >&2
    exit 1
  fi
fi

mkdir -p "$(dirname "$OUT")"

# --dry-run=client keeps the plaintext Secret off the cluster and out of the
# shell history; it is piped straight into kubeseal.
kubectl create secret generic "$NAME" \
  --namespace "$NS" \
  --dry-run=client -o yaml \
  --from-literal=INTEGRATION_CRED_KEY="$INTEGRATION_CRED_KEY" \
  --from-literal=SECRETS_ENCRYPTION_KEY="$SECRETS_ENCRYPTION_KEY" \
| kubeseal --format yaml > "$OUT"

echo "wrote ${OUT}"
echo "next: add it to the overlay's kustomization resources and apply"
