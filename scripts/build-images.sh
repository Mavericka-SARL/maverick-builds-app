#!/usr/bin/env bash
# Build every image the Kubernetes manifests run, from this checkout, and
# optionally push it to your own registry (docs/SELF_HOSTING.md).
#
#   scripts/build-images.sh REGISTRY TAG [--push] [--platform PLATFORM]
#
#   scripts/build-images.sh registry.example.com/mavericks 2026-09-25 --push
#
# REGISTRY is the prefix the images are named under: the gateway becomes
# REGISTRY/gateway:TAG. The list is read from deploy/k8s/base, so it is always
# the set the manifests run: the Go services (one generic Dockerfile), the web
# console, the two Postgres images (WAL-G, backups) and MinIO, which is built
# from source because MinIO publishes no pullable image any more.
#
# Build on a machine of the cluster's CPU architecture. --platform works, but
# a foreign architecture runs every compiler under emulation and takes hours.
#
# Prints, at the end, the `images:` block that points an overlay's copies of
# the base manifests at what was just built.
set -euo pipefail
cd "$(dirname "$0")/.."

usage() { sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
[ $# -ge 2 ] || usage
REGISTRY="${1%/}"; TAG="$2"; shift 2
PUSH=""; PLATFORM=""
while [ $# -gt 0 ]; do
  case "$1" in
    --push) PUSH=1 ;;
    --platform) PLATFORM="${2:?--platform needs a value, e.g. linux/amd64}"; shift ;;
    *) usage ;;
  esac
  shift
done

BASE=deploy/k8s/base
# The prefix the base manifests name their own images under, e.g.
# ghcr.io/<owner>/mavericks — what the overlay's images: entries must match.
BASE_PREFIX="$(grep -rhoE 'ghcr\.io/[^/ ]+/mavericks/' "$BASE" | sort -u | head -1)"
BASE_PREFIX="${BASE_PREFIX%/}"
[ -n "$BASE_PREFIX" ] || { echo "no ghcr.io/<owner>/mavericks images found under $BASE" >&2; exit 1; }
NAMES="$(grep -rhoE "${BASE_PREFIX//./\\.}/[a-z0-9-]+" "$BASE" | sed 's#.*/##' | sort -u)"
MINIO_REF="$(grep -rhoE 'quay\.io/minio/minio:[A-Za-z0-9._-]+' "$BASE" | sort -u | head -1 || true)"

build() { # name, context, dockerfile, extra docker build args...
  local name="$1" context="$2" file="$3"; shift 3
  local ref="${REGISTRY}/${name}:${TAG}"
  echo "==> building ${ref}" >&2
  docker build ${PLATFORM:+--platform "$PLATFORM"} -f "$file" -t "$ref" "$@" "$context"
  if [ -n "$PUSH" ]; then
    echo "==> pushing ${ref}" >&2
    docker push "$ref"
  fi
}

for name in $NAMES; do
  case "$name" in
    web)             build web web web/Dockerfile ;;
    postgres-backup) build postgres-backup deploy/docker/pg-backup deploy/docker/pg-backup/Dockerfile ;;
    postgres-walg)   build postgres-walg deploy/docker/postgres-walg deploy/docker/postgres-walg/Dockerfile ;;
    *)
      [ -d "cmd/$name" ] || { echo "base names image '$name' but there is no cmd/$name to build it from" >&2; exit 1; }
      build "$name" . deploy/docker/Dockerfile --build-arg "SERVICE=$name"
      ;;
  esac
done
if [ -n "$MINIO_REF" ]; then
  build minio deploy/docker/minio deploy/docker/minio/Dockerfile
fi

cat <<EOF

# Built${PUSH:+ and pushed}. For the overlay's kustomization.yaml:
images:
EOF
for name in $NAMES; do
  printf '  - name: %s/%s\n    newName: %s/%s\n    newTag: "%s"\n' "$BASE_PREFIX" "$name" "$REGISTRY" "$name" "$TAG"
done
if [ -n "$MINIO_REF" ]; then
  printf '  - name: %s\n    newName: %s/minio\n    newTag: "%s"\n' "${MINIO_REF%%:*}" "$REGISTRY" "$TAG"
fi
