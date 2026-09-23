#!/usr/bin/env bash
# Seals the mail relay's credentials for one cluster, as the `mavericks-smtp`
# Secret the gateway (e-mail notifications) and the backup watchdog (its
# alert) both read. Separate from scripts/seal-secrets.sh on purpose: a relay
# API key gets rotated on its own schedule, and rotating it must not mean
# re-sealing the database password alongside.
#
# The host, port and sender are not secrets and live in the mavericks-config
# ConfigMap (SMTP_HOST, SMTP_PORT, SMTP_FROM) — see docs/NOTIFICATIONS.md.
# Everything scripts/seal-secrets.sh says about sealed-secrets applies here:
# the output is bound to one cluster, is safe to commit, and belongs in that
# deployment's private repository.
#
# Usage (Resend: the username is the literal word "resend", the password an
# API key with sending permission on the verified domain):
#   SMTP_USERNAME=resend SMTP_PASSWORD=re_... bash scripts/seal-smtp-secret.sh
#
# Then add the output file to the overlay's kustomization.yaml, apply, and
# restart the gateway — it reads the relay once, at start-up. Prove it from
# Platform administration › Notification delivery › "Send a test e-mail":
# reading the configuration back shows only that it was stored.
set -euo pipefail

OUT="${OUT:-deploy/k8s/overlays/prod/sealed-smtp.yaml}"
NS="mavericks"
NAME="mavericks-smtp"

command -v kubeseal >/dev/null || { echo "kubeseal not found on PATH" >&2; exit 1; }
command -v kubectl  >/dev/null || { echo "kubectl not found on PATH" >&2; exit 1; }

ctx=$(kubectl config current-context)
echo "Sealing against context: ${ctx}"
read -r -p "Is that the cluster this relay is for? [y/N] " ok
[ "${ok:-n}" = "y" ] || { echo "aborted"; exit 1; }

# Read from the environment, never from arguments (visible in ps).
: "${SMTP_USERNAME:?missing required env var: SMTP_USERNAME}"
: "${SMTP_PASSWORD:?missing required env var: SMTP_PASSWORD}"

mkdir -p "$(dirname "$OUT")"

# --dry-run=client keeps the plaintext Secret off the cluster and out of the
# shell history; it is piped straight into kubeseal.
kubectl create secret generic "$NAME" \
  --namespace "$NS" \
  --dry-run=client -o yaml \
  --from-literal=SMTP_USERNAME="$SMTP_USERNAME" \
  --from-literal=SMTP_PASSWORD="$SMTP_PASSWORD" \
| kubeseal --format yaml > "$OUT"

echo "wrote ${OUT}"
echo "next: add it to the overlay's kustomization resources, apply, then"
echo "      kubectl -n ${NS} rollout restart deployment/gateway"
