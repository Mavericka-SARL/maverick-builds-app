# Self-hosted stack (Docker Compose)

> **Classification:** Current — The one-server deployment; docs/SELF_HOSTING.md is its manual.

maverickbuilds.app on a single server: the gateway, the console, the
integration worker, PostgreSQL, Keycloak, MinIO, a nightly backup and Caddy
for automatic HTTPS, built from this repository's source.

```bash
./setup.sh env                           # .env with generated secrets
$EDITOR .env                             # host names, e-mail relay
docker compose up -d --build
./setup.sh keycloak                      # sign-in service, user provisioning
./setup.sh admin you@example.com "Your Name"
./setup.sh check
```

Requirements, every setting, backups and restores, upgrades and
troubleshooting: [docs/SELF_HOSTING.md](../../docs/SELF_HOSTING.md).

This is not the development stack: `deploy/docker/docker-compose.dev.yml` runs
only the infrastructure, for `dev.sh` (docs/DEVELOPMENT.md).
