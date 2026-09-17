# The React console. Built and shipped separately from the Go services:
# deploy/docker/Dockerfile is one shared Go/distroless build parameterised by
# --build-arg SERVICE=<name>, and this shares none of it (node build, nginx
# runtime), so it gets its own Dockerfile and its own CI job — the same reason
# deploy/docker/pg-backup and postgres-walg have theirs.
#
# Build context is web/, not the repo root:
#   docker build -f web/Dockerfile -t web web/

FROM node:24-alpine AS builder
WORKDIR /app

# package files first so `npm ci` is cached independently of source edits.
COPY package.json package-lock.json ./
RUN npm ci

COPY . .
# Runs `tsc -b && vite build` — a type error fails the image build, which is
# deliberate: a bundle that doesn't typecheck should never reach a registry.
RUN npm run build

# nginx-unprivileged, not nginx: the k8s manifests run every pod as a non-root
# UID with a read-only root filesystem, and stock nginx assumes it can write
# its pid file and bind port 80 as root.
FROM nginxinc/nginx-unprivileged:1.27-alpine

COPY --from=builder /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
# /docker-entrypoint.d/*.sh is run by the base image before nginx starts.
COPY --chmod=755 docker-entrypoint.sh /docker-entrypoint.d/99-runtime-config.sh

EXPOSE 8080
