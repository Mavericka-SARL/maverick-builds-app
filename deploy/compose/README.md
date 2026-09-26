# Self-hosted stack (Docker Compose)

> **Classification:** Current — The one-server deployment; docs/SELF_HOSTING.md is its manual.

maverickbuilds.app on a single server: the gateway, the console, the
integration worker, PostgreSQL, Keycloak, MinIO, a nightly backup and Caddy
for automatic HTTPS — from a release's published images, or built from this
repository's source.

```bash
./setup.sh env                           # .env with generated secrets
$EDITOR .env                             # host names, e-mail relay, release
docker compose pull && docker compose up -d   # published images (amd64), or:
#   docker compose up -d --build              # build from source
./setup.sh keycloak                      # sign-in service, user provisioning
./setup.sh admin you@example.com "Your Name"
./setup.sh check
```

The published images are used when `.env` sets
`COMPOSE_FILE=docker-compose.yml:docker-compose.release.yml` and
`MAVERICKS_RELEASE` (see `docker-compose.release.yml`).

Requirements, every setting, backups and restores, upgrades and
troubleshooting: [docs/SELF_HOSTING.md](../../docs/SELF_HOSTING.md).

This is not the development stack: `deploy/docker/docker-compose.dev.yml` runs
only the infrastructure, for `dev.sh` (docs/DEVELOPMENT.md).
