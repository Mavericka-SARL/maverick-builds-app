# The React console. Built and shipped separately from the Go services:
# deploy/docker/Dockerfile is one shared Go/distroless build parameterised by
# --build-arg SERVICE=<name>, and this shares none of it (node build, nginx
# runtime), so it gets its own Dockerfile and its own CI job — the same reason
# deploy/docker/pg-backup and postgres-walg have theirs.
#
# Build context is web/, not the repo root, plus the repo's docs/ as a named
# context so the two manuals ship with the console (served at /docs/, see
# nginx.conf). From the repo root:
#   docker build -f web/Dockerfile --build-context docs=docs -t web web/

FROM node:24-alpine AS builder
WORKDIR /app

# package files first so `npm ci` is cached independently of source edits.
COPY package.json package-lock.json ./
RUN npm ci

COPY . .
# Runs `tsc -b && vite build` — a type error fails the image build, which is
# deliberate: a bundle that doesn't typecheck should never reach a registry.
RUN npm run build

# Guard: a --build-context named "docs" replaces this stage entirely, so it
# only runs when the flag was forgotten, and the build then fails saying so
# instead of shipping a console whose manual links answer 404. The base is the
# builder's, already pulled, so the guard adds no image of its own.
FROM node:24-alpine AS docs
RUN echo "web/Dockerfile needs the manuals: build with --build-context docs=<repo>/docs (see the header of web/Dockerfile)" >&2; exit 1

# nginx-unprivileged, not nginx: the k8s manifests run every pod as a non-root
# UID with a read-only root filesystem, and stock nginx assumes it can write
# its pid file and bind port 80 as root.
FROM nginxinc/nginx-unprivileged:1.27-alpine

COPY --from=builder /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
# The manuals, by explicit file: parts/, build.sh and the (gitignored) PDFs
# stay out. The layout is kept because the formulas manual's pictures are
# ../developer-manual/img/… relative to itself.
COPY --from=docs formulas-manual/manual.html  /usr/share/nginx/html/docs/formulas-manual/manual.html
COPY --from=docs developer-manual/manual.html /usr/share/nginx/html/docs/developer-manual/manual.html
COPY --from=docs developer-manual/img/        /usr/share/nginx/html/docs/developer-manual/img/
# /docker-entrypoint.d/*.sh is run by the base image before nginx starts.
COPY --chmod=755 docker-entrypoint.sh /docker-entrypoint.d/99-runtime-config.sh

EXPOSE 8080
