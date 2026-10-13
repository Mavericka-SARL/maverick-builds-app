# Self-hosting maverickbuilds.app

This guide takes you from an empty server to a running deployment that your
own people sign in to, with backups you have restored at least once. It is
written for whoever runs servers at your organisation: it assumes you know
your way around a Linux shell and DNS, and for the Kubernetes path, around
`kubectl`.

Everything runs on your infrastructure. Nothing calls home, and no license
key is needed: without one the platform runs the **Community Edition**, which
is the whole product except the features listed in
[LICENSING.md](LICENSING.md#gated-features) (single sign-on, SCIM, per-cell
history, audit export, usage analytics, tenant-level AI keys, white-labelling).
Read the [Sustainable Use License](../LICENSE) before you start: it permits
internal use only. Your own employees and contractors may use the platform;
nobody outside your company may sign in to it, and hosting it for others,
building on it for clients or reselling it needs a Commercial License.
The gated features' code is in every image and stays switched off without a
key, as its [Enterprise Features License](../ee/LICENSE) permits.

## Choose a path

| | [Docker Compose, one server](#install-on-one-server-with-docker-compose) | [Kubernetes](#install-on-kubernetes) |
|---|---|---|
| What runs | 8 containers: the gateway, the web console, the integration worker, PostgreSQL, Keycloak, MinIO, a nightly backup job, and Caddy for HTTPS | The gateway (two replicas), the web console and the integration worker, plus PostgreSQL with continuous WAL archiving, PgBouncer, Keycloak and MinIO |
| Good for | One organisation, a team to a few hundred users; the simplest thing to keep running | Several nodes, rolling upgrades, point-in-time recovery |
| You need | One Linux server and Docker, or a Windows computer with [WSL 2](#on-windows) | A cluster with Traefik and cert-manager, and a container registry |
| Time to first sign-in | About an hour, most of it building | Half a day |

Both paths build the containers **from this repository's source**; there is
no public image registry to pull them from. Both run the same code, the same
database schema and the same sign-in service, so moving from one to the other
later is a backup and a restore.

---

## Requirements

### For either path

- **Two DNS names** pointing at the deployment: one for the console and its
  API (say `mavericks.example.com`) and one for the sign-in service
  (`auth.mavericks.example.com`). Sign-in is Keycloak, and it runs on its own
  host name.
- **An SMTP relay.** This is not optional: an administrator adds a user by
  having the platform e-mail them a link to set their own password, and when
  the e-mail cannot be sent the user is not created. The same relay carries
  notifications and task reminders. Any relay works — your organisation's own,
  or a sending service such as Amazon SES, Postmark or Resend — with or without
  authentication, on port 587 (STARTTLS) or 465 (TLS). An internal relay on
  port 25 works with Docker Compose; on Kubernetes the network policies only
  let 587 and 465 out.
- **Somewhere else to keep backups**: an S3-compatible bucket that does not
  live on the same machine or cluster as the database. The platform backs up
  on its own by default, but to storage next to the data, which survives a
  mistake and not the loss of the server.
- **Internet access while building**, to fetch base images, Go modules, npm
  packages and the MinIO source. Once built, the platform itself only calls out
  for the features that need it (AI providers, Google Sheets, REST connectors
  your users configure, and your SMTP relay).
- On the machine you install from: `git`, `bash`, `curl`, `openssl` and
  `python3` (the setup scripts use it to read JSON).

### For Docker Compose

| | Minimum | Recommended |
|---|---|---|
| CPU | 2 cores | 4 cores |
| Memory | 4 GB | 8 GB |
| Disk | 30 GB | 60 GB or more, depending on your data |
| OS | 64-bit Linux, x86-64 or arm64; or Windows 10 (22H2) or 11 through WSL 2, see [On Windows](#on-windows) | a current Ubuntu or Debian LTS release |
| Software | Docker Engine with the Compose v2 plugin, 2.24 or later (`docker compose version`) | |

The running platform idles at about 1 GB of memory, three quarters of it
Keycloak. Building needs more: the Go and Node.js compilers want 3–4 GB free
while they run, which is the reason for the 4 GB minimum.

Ports **80 and 443** must be free on the server and, for certificates from
Let's Encrypt, reachable from the internet. Nothing else is published.

### For Kubernetes

- A supported Kubernetes release. This guide was verified on 1.37.
- **Traefik**, installed in a namespace named `traefik` with
  [`deploy/k8s/bootstrap/traefik-values.yaml`](../deploy/k8s/bootstrap/traefik-values.yaml)
  (step 1): the Ingress uses its `traefik` class, and the network policies
  admit sign-in traffic to Keycloak from that namespace. To use another
  controller instead, change `ingressClassName` in your overlay and the
  `keycloak` policy's namespace in
  [`deploy/k8s/base/networkpolicy.yaml`](../deploy/k8s/base/networkpolicy.yaml),
  and give it an HTTP-to-HTTPS redirect that leaves cert-manager's challenge
  path alone. (Until 2026-09 these manifests used ingress-nginx, whose
  upstream project was retired in March 2026.)
- **cert-manager**, for certificates (or your own TLS secrets).
- A **StorageClass** with `ReadWriteOnce` volumes: 20 GiB for PostgreSQL
  and, if MinIO runs in the cluster, 20 GiB for it.
- A network plugin that **enforces NetworkPolicy** (Calico, Cilium, k3s's
  built-in controller, kind's kindnet). Without enforcement the policies are
  ignored rather than harmful, and you lose their isolation.
- A **container registry** the cluster can pull from, and Docker with buildx
  on a machine of the cluster's CPU architecture, to build the 19 images.
- Capacity: the 23 pods request 1.6 CPU and 2.5 GiB of memory and may grow to
  their limits, about 8 CPU and 9 GiB, under load. A single node of 4 CPU /
  8 GB runs the whole platform for evaluation (idle, it used 2.7 GB including
  Kubernetes itself); three such nodes are a sensible floor for production.

---

## Install on one server with Docker Compose

The stack lives in [`deploy/compose/`](../deploy/compose/):

| Container | Role |
|---|---|
| `proxy` | Caddy: the only published ports, HTTPS with automatic certificates, routing by host name |
| `web` | The console (static files) |
| `gateway` | The API and every background job: calculations, workflows, notifications, schedules |
| `integration` | Runs REST API connector syncs and SFTP file imports |
| `keycloak` | Sign-in, passwords, invitations |
| `postgres` | PostgreSQL 16: the platform's database, Keycloak's database, and one database per tenant if you choose that |
| `minio` | Object storage for the backups, by default |
| `backup` | Dumps every database once a day |

### On Windows

The stack runs on Windows 10 (22H2) or 11 inside **WSL 2**, the Linux that
ships with Windows. Do this first, then follow steps 2 to 8 **in the Ubuntu
terminal**, never in PowerShell or Git Bash: `setup.sh` is a bash script that
needs `python3`, and the containers are Linux containers either way.

1. In PowerShell, as administrator, install WSL 2 with Ubuntu, and restart
   when it asks:

   ```powershell
   wsl --install -d Ubuntu
   ```

   Open **Ubuntu** from the Start menu once and choose a Linux user name and
   password.
2. Install [Docker Desktop](https://docs.docker.com/desktop/setup/install/windows-install/)
   with its default WSL 2 back end. In **Settings › Resources › WSL
   integration**, turn on **Ubuntu**; in **Settings › General**, turn on
   **Start Docker Desktop when you sign in**, because the platform, and its
   nightly backup, run only while Docker Desktop does. Docker Desktop needs a
   paid Docker subscription in larger companies; Docker Engine installed
   inside Ubuntu, [as on Linux](https://docs.docker.com/engine/install/ubuntu/),
   works as well.
3. In the Ubuntu terminal, check that `docker compose version` and
   `python3 --version` both answer.
4. Run `cd ~` before Step 2, so that the clone lives in Ubuntu's own file
   system. A clone under `/mnt/c` builds many times slower and ignores the
   file permissions `setup.sh` puts on `.env`.

On a typical Windows PC (x86-64), Step 4's published images save the build.
Docker Desktop gives Linux half of the computer's memory; on an 8 GB computer
that is too little to build from source.

To serve other people, the computer needs the DNS records and open ports of
Step 1. For a trial on the computer itself, set `CADDY_CERTS=local_certs`
(Step 3) and point both names at it in
`C:\Windows\System32\drivers\etc\hosts`, edited as administrator — for example:

```text
127.0.0.1  mavericks.test auth.mavericks.test
```

Browsers warn about the certificates until Caddy's root certificate is
trusted. After Step 4, from `deploy/compose` in the Ubuntu terminal:

```bash
docker compose cp proxy:/data/caddy/pki/authorities/local/root.crt .
explorer.exe .
```

In the folder that opens, double-click `root.crt`, choose **Install
Certificate › Local Machine**, and place it in **Trusted Root Certification
Authorities**. Edge and Chrome use it at once.

### Step 1 — Prepare the server

1. Install Docker Engine and the Compose plugin from
   [Docker's instructions for your distribution](https://docs.docker.com/engine/install/),
   and check that `docker compose version` answers.
2. Create DNS records for both names — `A` (and `AAAA`, if the server has
   IPv6) — pointing at the server.
3. Open ports 80 and 443 in the server's firewall and in any cloud firewall in
   front of it. Keep SSH restricted to your own addresses.

**Check:** `dig +short mavericks.example.com` and
`dig +short auth.mavericks.example.com` both return the server's address.
Certificates cannot be issued until they do.

### Step 2 — Get the code

```bash
git clone https://github.com/Mavericka-SARL/maverick-builds-app.git maverickbuilds
cd maverickbuilds
git checkout 2026.10.13        # a release: the newest is on the Releases page
cd deploy/compose
```

Keep the clone: it holds the Compose files and `setup.sh`, and upgrading is
checking out a newer release in it (see [Upgrading](#upgrading)).

### Step 3 — Configure

```bash
./setup.sh env
```

This creates `.env` from [`.env.example`](../deploy/compose/.env.example),
readable only by you, and fills in every secret with a random value. Then edit
`.env` and set at least:

| Setting | Value |
|---|---|
| `CONSOLE_HOST` | The console's host name, e.g. `mavericks.example.com` — no `https://` |
| `AUTH_HOST` | The sign-in host name, e.g. `auth.mavericks.example.com` |
| `SMTP_HOST`, `SMTP_PORT` | Your relay; 587 for STARTTLS, 465 for TLS, 25 for an internal relay |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | The relay's credentials; leave both empty for a relay without authentication |
| `SMTP_FROM` | The sender, on a domain the relay is allowed to send for, e.g. `no-reply@example.com` |

Two choices are easier to make now than later:

- **`TENANT_DB_MODE`** — `shared` (the default) keeps every tenant in one
  database; `dedicated` gives each tenant a database of its own on the same
  server ([TENANT_DATABASES.md](TENANT_DATABASES.md)). Switching later only
  affects tenants created afterwards. With `dedicated`, a platform
  administrator works in every tenant as platform administrator only, so
  the first administrator cannot take a tenant as their own (step 8):
  invite a second account to hold its roles.
- **`CADDY_CERTS`** — empty for Let's Encrypt certificates, which needs both
  names publicly resolvable and ports 80/443 reachable from the internet. On a
  private network, set it to `local_certs`: Caddy then issues certificates from
  its own authority, which browsers warn about until its root certificate is
  installed on them (`docker compose cp proxy:/data/caddy/pki/authorities/local/root.crt .`).

Every other setting is optional and explained in `.env.example`.

**Save a copy of `.env` in a password manager now.** It holds the database
password and the two keys (`INTEGRATION_CRED_KEY`, `SECRETS_ENCRYPTION_KEY`)
that tenants' stored credentials are encrypted under. A backup restored
without them opens, but every stored connector password and API key in it is
unreadable.

### Step 4 — Start

On an x86-64 (amd64) server, run the release's published images. Add two
lines to `.env`:

```bash
COMPOSE_FILE=docker-compose.yml:docker-compose.release.yml
MAVERICKS_RELEASE=2026.10.13   # the release you checked out
```

```bash
docker compose pull
docker compose up -d
```

On any other architecture (an ARM server, an Apple-silicon Mac), leave those
two lines out and build from source instead:

```bash
docker compose up -d --build
```

The first build takes 5–15 minutes, depending on the machine. Later builds
reuse most of the work.

**Check:** `docker compose ps` lists eight containers, all `running`, and
`keycloak`, `postgres`, `web` and `minio` report `(healthy)`. The gateway
waits for Keycloak and may take a minute longer.

### Step 5 — Finish setting up sign-in

```bash
./setup.sh keycloak
```

This, in order:

1. gives the gateway the Keycloak service account it creates users with, and
   stores that account's secret in `.env`;
2. turns on brute-force protection for Keycloak's administrator realm (the
   platform's own realm has it from the start);
3. configures Keycloak's e-mail from your `SMTP_*` settings, for invitations
   and password resets;
4. restarts the gateway with the new secret.

Add `SMTP_TEST_TO=you@example.com` in front of the command to have Keycloak
send a real test message. Do: a wrong relay password or a sender the relay
refuses shows up only when something tries to send.

The script talks to the stack through the proxy on the server itself, using
your real host names, so it works before public DNS has propagated.

### Step 6 — Create the first administrator

A fresh deployment has nobody who can sign in, and only a signed-in
administrator can add people. Create the first one:

```bash
./setup.sh admin you@example.com "Your Name"
```

It asks for a password (12 characters or more). This person is a **platform
administrator**: they manage tenants, applications and users across the
whole deployment. Their first sign-in sets up their own organisation, with
every other role in it ([step 8](#step-8--first-steps-in-the-console)).

### Step 7 — Check

```bash
./setup.sh check
```

```text
Through the proxy:
  ok    console
  ok    gateway (refuses an anonymous call)
  ok    sign-in service
Configuration:
  ok    user provisioning configured
  ok    e-mail relay set (smtp.example.com)
Backups:
  none yet — the first runs at 3:00 UTC; take one now with:
        docker compose exec backup backup-schedule.sh now
```

Then open `https://mavericks.example.com` in a browser. It sends you to the
sign-in page; sign in as the administrator from step 6.

### Step 8 — First steps in the console

The platform is organised as **tenants** (the organisations using it — often
just your own), each holding **applications**, each holding **models** that
developers build and users work in. People are given
**roles**:

| Role | Does |
|---|---|
| `platform_admin` | Everything, across all tenants: tenants, applications, users, platform settings |
| `tenant_admin` | Manages one tenant's workspaces, applications and users |
| `developer` | Builds models: dimensions, metrics, grids, forms, dashboards, workflows, integrations |
| `business_admin` | Administers business consoles within a workspace |
| `business_user` | Enters and reviews data |

A self-hosted deployment starts where a sign-up on the hosted Playground
does, without the Playground's 100 MB limit. As the platform administrator:

1. **Applications › New tenant** — name your organisation. Leave both boxes
   ticked:
   - **Getting started application** — the *Learn the platform* tour and a
     guide for each role (developer, business administrator, tenant
     administrator), the same as every Playground workspace gets. New
     releases bring them up to date.
   - **Make it my own, with every role** — you become the tenant's
     administrator, developer, business administrator and user, beside
     platform administrator. It is offered while your account has no tenant
     of its own, with the default single database.

   The console starts again and opens on the tour. Every tenant you create
   later gets the Getting started application too, unless you untick it.
   No edition carries a limit here — the Community Edition's 100 MB is the
   Playground's alone, and the gateway takes it off at start-up anywhere
   else. To set limits of your own, edit an edition under **Platform ›
   Editions**.
2. **Users › Invite user** — e-mail, name and a role, e.g. `developer`. The
   person receives an e-mail with a link to set their password; the link is
   valid for three days. Business roles are also given a workspace.
3. To build your own application: **New application** in the tenant, then
   **Add model** inside it. The developer finds the model under **Models**,
   creates its first revision and starts building.

The Playground runs the Enterprise Edition. Single sign-on, SCIM, cell
history, audit export, usage analytics, white-labelling, tenant AI keys and
deployment settings need a Commercial or Enterprise licence key here
(**Platform › License**). Everything else works as it does there. The console's screens are described in the
[developer manual](developer-manual/), and the formula language in the
[formulas manual](formulas-manual/). Your deployment serves both itself, at
`https://<console host>/docs/developer-manual/manual.html` and
`/docs/formulas-manual/manual.html`: the web image carries them. A web image
built by hand needs `--build-context docs=docs --build-context legal=.` (see
`web/Dockerfile`; the second carries the licence files it serves at
`/licenses/`); `scripts/build-images.sh` and Compose pass both for you.

Create people through the console, not in Keycloak's own admin console: the
platform keeps its own record of every user and their roles, and only the
console creates both.

---

## Operating a Compose deployment

All commands run in `deploy/compose/`.

### Backups

Every day at `BACKUP_HOUR` (UTC, 3:00 by default) the `backup` container
dumps the platform's database, every tenant database and Keycloak's database
— all three are needed to restore a working deployment — and keeps them for
`BACKUP_RETENTION_DAYS` (14). All dumps of one pass share a timestamp, called
a *run* below.

```bash
docker compose exec backup backup-schedule.sh now   # take one now
./setup.sh check                                     # lists the newest dumps
```

By default the dumps go to the stack's own MinIO, on the same server. To keep
them somewhere a lost server does not take with it, point the backup settings
in `.env` at another S3-compatible bucket and apply them:

```bash
# in .env
BACKUP_S3_URL=https://s3.eu-central-1.amazonaws.com
BACKUP_S3_ACCESS_KEY=...
BACKUP_S3_SECRET_KEY=...
BACKUP_BUCKET=example-maverickbuilds-backups
BACKUP_REGION=eu-central-1      # only if the bucket does not exist yet

docker compose up -d backup
docker compose exec backup backup-schedule.sh now   # prove it
```

Set `ALERT_EMAIL` in `.env` (and `docker compose up -d backup`) to be told
when a run fails or leaves a dump smaller than `BACKUP_MIN_SIZE` (100 KB):
after every run the container checks what actually arrived in the bucket and
mails that address through your relay, which for this has to offer TLS.
The `disk-watchdog` service mails the same address when the database
volume passes 80% (90% critical), so it is grown before Postgres stops:
`docker compose up -d disk-watchdog`.

Keep these outside the dumps as well:

- **`.env`** — see step 3.
- Nothing else is needed. Certificates are issued again by themselves, and
  the model packages in MinIO can be exported again.

### Restoring

```bash
./setup.sh restore                      # lists the runs
./setup.sh restore 20260925T030000Z     # or: ./setup.sh restore latest
```

It asks you to type `RESTORE`, stops everything that writes to the
databases, replaces **every** database in the run with its dump — so the
platform, its tenants and the sign-in accounts come back from the same moment
— and starts the stack again. Everything written after the run is gone,
including users invited since.

Restore once before you need to: take a backup, add something (a tenant, a
user), restore, and check that the addition is gone and that everyone from
before can still sign in. A backup that has never been restored is a hope.

To move to a new server: install it (steps 1–4) with the **same `.env`**,
copy the backups into its bucket or point it at the external one, and
restore.

### Upgrading

```bash
docker compose exec backup backup-schedule.sh now   # a restore point
git fetch --tags && git checkout <new release>       # e.g. 2026.10.15
# published images: set MAVERICKS_RELEASE=<new release> in .env, then
docker compose pull && docker compose up -d
# built from source instead:
#   docker compose up -d --build
./setup.sh check
```

The gateway applies database migrations when it starts. They only go
forward: to go back, check out the previous release (and set it back in
`.env`) **and** restore the backup taken before the upgrade. Old images and
build cache accumulate; `docker image prune` and `docker builder prune`
reclaim the space.

### Changing settings

Edit `.env`, then `docker compose up -d`: Compose recreates exactly the
containers whose settings changed.

- **A license key** for the Commercial or Enterprise Edition goes in
  `MAVERICKS_LICENSE_KEY`. **Platform › License** in the console shows the
  edition it unlocked.
- **The AI assistant** uses each user's own provider key, stored in the
  console. A deployment-wide key (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, …)
  is used by everyone who has not stored one ([AI_KEYS.md](AI_KEYS.md)).
- **Public sign-up** is off; administrators create tenants and users. To let
  visitors create their own workspace, see
  [EDITIONS_AND_SIGNUP.md](EDITIONS_AND_SIGNUP.md), and publish your terms and
  privacy notice first ([LEGAL_AND_PRIVACY.md](LEGAL_AND_PRIVACY.md)).

**Changing a host name** takes one more step. Keycloak reads its realm file
only on first start, so it keeps the old console address as the only place it
may send people back to. After changing `CONSOLE_HOST` and running
`docker compose up -d`, open Keycloak's admin console (next section), choose
the `mavericks` realm › **Clients** › `mavericks-web`, and change the
addresses under **Valid redirect URIs**, **Valid post logout redirect URIs**
and **Web origins**. A changed `AUTH_HOST` needs nothing more than
`docker compose up -d`.

### Keycloak's admin console

`https://auth.mavericks.example.com/admin`, user `admin`, password
`KEYCLOAK_ADMIN_PASSWORD` from `.env`. Use it to reset a password, to unlock
an account that brute-force protection locked, or to change sign-in policy.

It is reachable by anyone who can reach the sign-in host. If your
administrators work from fixed addresses, restrict `/admin` to them in the
`Caddyfile`:

```text
{$AUTH_HOST} {
	@admin {
		path /admin*
		not remote_ip 203.0.113.0/24
	}
	respond @admin 403
	reverse_proxy keycloak:8080
}
```

### Logs

```bash
docker compose logs -f gateway          # or keycloak, proxy, backup, ...
docker compose logs --since 1h
```

Each container keeps five log files of 20 MB.

### Troubleshooting

| Symptom | Cause and fix |
|---|---|
| The browser says the certificate is invalid; `docker compose logs proxy` shows ACME errors | Let's Encrypt cannot reach the server: a DNS record is missing or wrong, or port 80 is closed. Fix it and restart the proxy: `docker compose restart proxy`. |
| Sign-in says **Invalid parameter: redirect_uri** | The console's address is not one Keycloak allows — usually `CONSOLE_HOST` changed after the first start. See *Changing a host name*. |
| The gateway restarts over and over; its log says `failed to initialize JWKS validator` | It cannot reach Keycloak's realm. Check `docker compose logs keycloak`; it may still be starting. |
| Every API call answers 401 after signing in | The token's issuer is not `https://AUTH_HOST`: `AUTH_HOST` was edited without `docker compose up -d`, or Keycloak is reached through another name. |
| **Invite user** answers 503 | The provisioning account is not configured: run `./setup.sh keycloak`. |
| **Invite user** answers 502 and names the e-mail step | Keycloak could not send the invitation. Rerun `SMTP_TEST_TO=you@example.com ./setup.sh keycloak` and read what the relay says. |
| Keycloak stops with `database "keycloak" does not exist` | The database volume was created before the stack's init script ran. Create it: `docker compose exec postgres psql -U mavericks -c 'CREATE DATABASE keycloak OWNER mavericks'`, then `docker compose up -d`. |
| The `web` container restarts over and over; its log says `99-runtime-config.sh: not found` | The clone has Windows (CRLF) line endings: Git for Windows made it, from a release before 2026.10.12.1. Repair it in place from `deploy/compose`, in the Ubuntu terminal: `git config core.autocrlf false`, then `(cd ../.. && git rm -rq --cached . && git reset -q --hard)` and `sed -i 's/\r$//' .env`, then `docker compose up -d --build`. |
| `./setup.sh check` shows no backups the day after installing | `docker compose logs backup` names the failing step; for an external bucket it is usually the credentials or `BACKUP_REGION`. |

---

## Install on Kubernetes

The manifests are in [`deploy/k8s/base/`](../deploy/k8s/base/); your
deployment is an **overlay** on top of them, which you start from
[`deploy/k8s/overlays/prod.example/`](../deploy/k8s/overlays/prod.example/).
The steps below name the namespace `mavericks`, the base default.

### Step 1 — Prepare the cluster

Install Traefik and cert-manager:

```bash
helm install traefik oci://ghcr.io/traefik/helm/traefik --version 41.6.0 \
  -n traefik --create-namespace -f deploy/k8s/bootstrap/traefik-values.yaml
helm repo add jetstack https://charts.jetstack.io && helm repo update
helm install cert-manager jetstack/cert-manager -n cert-manager --create-namespace --set crds.enabled=true
```

The values file sets what the platform needs from its entry point: a
`traefik` IngressClass, HTTP redirected to HTTPS except cert-manager's
challenge path, a 300-second limit for reading a request (large imports over a
slow link), and two replicas with a disruption budget. Your cloud's
load-balancer settings go in a second values file; the comment in
`service.annotations` shows its shape. Keep its PROXY-protocol part if your
load balancer forwards plain TCP, as most do: the gateway throttles sign-ups
and SSO discovery per client address, and without the PROXY protocol every
visitor arrives from the same internal one.

Point both DNS names at Traefik's external address
(`kubectl -n traefik get svc traefik`). Then create the Let's Encrypt issuers,
after replacing `ops@example.com` in the file with your address:

```bash
kubectl apply -f deploy/k8s/bootstrap/cert-manager-issuer.yaml
```

On a private network, where Let's Encrypt cannot reach the ingress, create
a cert-manager
[CA issuer](https://cert-manager.io/docs/configuration/ca/) from your
organisation's certificate authority instead and name it in the overlay (step
3), or create the TLS secrets `mavericks-tls` and `mavericks-auth-tls`
yourself and remove the issuer annotation.

**Check:** `kubectl get storageclass` shows the class you will use, and
`kubectl get clusterissuer` shows `letsencrypt-staging` and `letsencrypt-prod`
as `READY`.

### Step 2 — Choose the images

Every release publishes its images, built for x86-64 (amd64), at
`ghcr.io/mavericka-sarl/mavericks/<name>:<release>`, and the example overlay
already points at them. On amd64 nodes, skip to step 3.

To run images you build yourself — ARM nodes, your own changes, or a
registry you control — use a machine with Docker, of the same CPU
architecture as the cluster's nodes, logged in to your registry
(`docker login registry.example.com`):

```bash
scripts/build-images.sh registry.example.com/mavericks 2026-09-25 --push
```

The first argument is where the images go, the second their tag; use the
date or the revision you built. The script builds all 19 images and ends by
printing an `images:` block — keep it for the next step.

If the registry is private, give the cluster its credentials:

```bash
kubectl create namespace mavericks
kubectl -n mavericks create secret docker-registry registry-pull \
  --docker-server=registry.example.com --docker-username=... --docker-password=...
kubectl -n mavericks patch serviceaccount default -p '{"imagePullSecrets":[{"name":"registry-pull"}]}'
```

### Step 3 — Create your overlay

```bash
cp -r deploy/k8s/overlays/prod.example deploy/k8s/overlays/prod
```

`deploy/k8s/overlays/prod/` is ignored by git in this repository, so nothing
in it can be committed here by accident; keep a copy in a private repository
of your own. Then edit it:

1. **`site.yaml`** — every line marked `CHANGE`: the two host names (they
   appear several times and must agree everywhere), the storage class, the
   SMTP relay and sender, and the address the backup watchdog alerts.
2. **`kustomization.yaml`** — its `images:` block pins every image to the
   release `2026.10.13`; set the release you deploy, or, if you built your
   own images, replace the block with the one `build-images.sh` printed.
3. **Object storage.** By default MinIO runs in the cluster and holds the
   backups and WAL archive. To keep them outside the cluster, as you should in
   production: set `MINIO_URL` (the endpoint, `https://`) and `MINIO_ROOT_USER`
   (the access key) in `site.yaml`'s `mavericks-config`; set the bucket's region as `AWS_REGION` on both
   containers of the `postgres` StatefulSet if it is not `us-east-1`; and
   uncomment `minio-remove.yaml` and `networkpolicy-external-objectstore.yaml`
   in `kustomization.yaml`, deleting the `minio-data` patch from `site.yaml`.

**Check:** `kubectl kustomize deploy/k8s/overlays/prod > /dev/null` builds
without errors, and `kubectl kustomize deploy/k8s/overlays/prod | grep image:`
names only your registry, `quay.io/keycloak/keycloak` and
`edoburu/pgbouncer`.

### Step 4 — Create the secrets

```bash
kubectl create namespace mavericks        # if step 2 did not

# MINIO_ROOT_PASSWORD: a new random value for in-cluster MinIO, as here; with
# external object storage, that storage's secret key instead.
kubectl -n mavericks create secret generic mavericks-secrets \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 32)" \
  --from-literal=KEYCLOAK_ADMIN_PASSWORD="$(openssl rand -hex 32)" \
  --from-literal=MINIO_ROOT_PASSWORD="$(openssl rand -hex 32)" \
  --from-literal=KEYCLOAK_ADMIN_CLIENT_SECRET=pending

kubectl -n mavericks create secret generic mavericks-integration \
  --from-literal=INTEGRATION_CRED_KEY="$(openssl rand -hex 32)" \
  --from-literal=SECRETS_ENCRYPTION_KEY="$(openssl rand -hex 32)"

# Only for a relay that needs a user name and password:
kubectl -n mavericks create secret generic mavericks-smtp \
  --from-literal=SMTP_USERNAME=... --from-literal=SMTP_PASSWORD=...
```

Then read them back and **store every value in a password manager**
(`kubectl -n mavericks get secret mavericks-secrets -o yaml`, values are
base64). The same warning as for Compose applies: backups are only readable
with `POSTGRES_PASSWORD`, and tenants' stored credentials only with the two
keys in `mavericks-integration`. Never regenerate those two on a running
deployment.

If you keep your overlay in git and want the secrets there too, seal them
with [sealed-secrets](https://github.com/bitnami-labs/sealed-secrets) instead:
`scripts/seal-secrets.sh`, `scripts/seal-integration-secret.sh` and
`scripts/seal-smtp-secret.sh` produce files to list in the overlay's
`resources:`.

A license key, if you have one, goes in a secret too:
`kubectl -n mavericks create secret generic mavericks-license --from-literal=key=MVX1...`.

### Step 5 — Deploy

```bash
kubectl apply -k deploy/k8s/overlays/prod
```

Keycloak keeps its data in a database of its own, which PostgreSQL does not
create by itself. Once `postgres-0` is running:

```bash
kubectl -n mavericks wait --for=condition=ready pod/postgres-0 --timeout=5m
kubectl -n mavericks exec postgres-0 -c postgres -- \
  psql -U mavericks -d mavericks -c 'CREATE DATABASE keycloak OWNER mavericks'
kubectl -n mavericks rollout restart deployment/keycloak
```

The gateway applies database migrations itself when it starts.

**Check:** `kubectl -n mavericks get pods` — everything `Running` and ready
within a few minutes, and nothing in `CrashLoopBackOff`. A pod in
`CreateContainerConfigError` is missing a secret from step 4.
`kubectl -n mavericks get certificate` shows both certificates `READY`.

### Step 6 — Finish setting up sign-in

The same three scripts `./setup.sh keycloak` runs for Compose, run by hand
from the repository's root:

```bash
export KEYCLOAK_URL=https://auth.mavericks.example.com
export KEYCLOAK_ADMIN_PASSWORD="$(kubectl -n mavericks get secret mavericks-secrets -o jsonpath='{.data.KEYCLOAK_ADMIN_PASSWORD}' | base64 -d)"

# 1. The provisioning account; store its secret and restart the gateway.
SECRET="$(bash scripts/keycloak-provisioning-setup.sh)"
kubectl -n mavericks patch secret mavericks-secrets \
  -p "{\"stringData\":{\"KEYCLOAK_ADMIN_CLIENT_SECRET\":\"$SECRET\"}}"
kubectl -n mavericks rollout restart deployment/gateway

# 2. Keycloak's e-mail, with a real test message.
SMTP_HOST=smtp.example.com SMTP_PORT=587 SMTP_USER=... SMTP_PASSWORD=... \
SMTP_FROM=no-reply@example.com SMTP_TEST_TO=you@example.com \
  bash scripts/keycloak-smtp-setup.sh

# 3. Brute-force protection for the administrator realm.
bash scripts/keycloak-master-hardening.sh
```

While the certificates are still from `letsencrypt-staging`, or come from your
own CA, add `export CURL_OPTS=-k` first; every script here passes it to
`curl`. For a relay without authentication, set `SMTP_USER=` (empty) and no
password.

### Step 7 — Create the first administrator

```bash
ADMIN_EMAIL=you@example.com ADMIN_PASSWORD='...' ADMIN_NAME="Your Name" \
PSQL="kubectl -n mavericks exec -i postgres-0 -c postgres -- psql -U mavericks -d mavericks" \
  bash scripts/bootstrap-platform-admin.sh
```

(`KEYCLOAK_URL` and `KEYCLOAK_ADMIN_PASSWORD` from step 6 still set.)

### Step 8 — Switch to real certificates, and sign in

The overlay starts on Let's Encrypt's staging service, whose certificates
browsers do not trust but whose rate limits a misconfiguration cannot exhaust.
Once `kubectl -n mavericks get certificate` shows both `READY`, change the
`cert-manager.io/cluster-issuer` annotation in `site.yaml` to
`letsencrypt-prod`, and let cert-manager issue them again:

```bash
kubectl apply -k deploy/k8s/overlays/prod
kubectl -n mavericks delete secret mavericks-tls mavericks-auth-tls
```

Then sign in at `https://mavericks.example.com` and continue with
[first steps in the console](#step-8--first-steps-in-the-console).

---

## Operating a Kubernetes deployment

### Backups

Two mechanisms, both writing to the object storage configured in step 3:

- **Continuous WAL archiving** (WAL-G, inside the `postgres` pod) plus a daily
  base backup, for point-in-time recovery of the whole server. List them with
  `kubectl -n mavericks exec postgres-0 -c postgres -- wal-g backup-list`.
- **Daily dumps** (the `postgres-backup` CronJob, 03:00): the platform's
  database, every tenant database and — with the example overlay's setting
  `BACKUP_DATABASES: all,keycloak` — Keycloak's, one object each under `pg/`
  in the `mavericks-backups` bucket. Take one now and read how it went:

  ```bash
  kubectl -n mavericks create job --from=cronjob/postgres-backup backup-now
  kubectl -n mavericks logs -f job/backup-now      # ends with "backup complete"
  ```

The `postgres-backup-watchdog` CronJob checks every morning that a dump of the
control database newer than 26 hours exists and that `pg_restore` reads it
through to the end, and e-mails `ALERT_EMAIL` when either is not so. Prove it once:
`kubectl -n mavericks create job --from=cronjob/postgres-backup-watchdog watchdog-now`,
then `kubectl -n mavericks logs job/watchdog-now` ends with `ok:`.

The `disk-watchdog` container in the `postgres-0` pod mails the same address
before the database volume fills: at 80% (warning) and 90% (critical), and
when more than 20 WAL segments, or any older than an hour, are waiting to be
archived. It repeats daily while a problem lasts and mails once when it is
over. `DISK_WARN_PERCENT`, `DISK_CRITICAL_PERCENT`, `WAL_MAX_READY` and
`WAL_MAX_READY_MINUTES` in `mavericks-config` move the lines. Prove it with a
line it is always past, which sends a real mail:
`kubectl -n mavericks exec postgres-0 -c disk-watchdog -- env ONCE=1 DISK_WARN_PERCENT=0 /usr/local/bin/disk-watchdog.sh`.
To grow the volume, patch the claim, not the StatefulSet, whose
`volumeClaimTemplates` cannot change in place:
`kubectl -n mavericks patch pvc postgres-data-postgres-0 -p '{"spec":{"resources":{"requests":{"storage":"50Gi"}}}}'`
(the storage class must allow expansion; volumes cannot shrink).

To restore dumps: stop everything that writes, restore each database, and
start again. `pg-restore.sh` drops the database and loads the dump into an
empty one; `latest` is the newest dump of the database the URL names:

```bash
IMG=ghcr.io/mavericka-sarl/mavericks/postgres-backup:2026.10.13   # the one your overlay runs
kubectl -n mavericks scale deployment --replicas=0 \
  -l 'app.kubernetes.io/component in (api-gateway,service,identity)'
for db in mavericks keycloak; do          # and each tenant_… database, if dedicated
  kubectl -n mavericks run pg-restore --rm -i --restart=Never --image=$IMG \
    --labels=app=postgres-backup,app.kubernetes.io/part-of=mavericks-engine,app.kubernetes.io/managed-by=kustomize \
    --overrides='{"spec":{"containers":[{"name":"pg-restore","image":"'$IMG'","command":["pg-restore.sh","latest"],"env":[{"name":"RESTORE_DATABASE_URL","value":"postgres://mavericks:$(POSTGRES_PASSWORD)@postgres:5432/'$db'?sslmode=disable"},{"name":"CONFIRM_RESTORE","value":"yes"}],"envFrom":[{"configMapRef":{"name":"mavericks-config"}},{"secretRef":{"name":"mavericks-secrets"}}]}]}}'
done
kubectl apply -k deploy/k8s/overlays/prod      # restores the replica counts
```

The three labels are what let the pod through the network policies to
PostgreSQL and MinIO — with fewer, it times out before it touches anything.
`$(POSTGRES_PASSWORD)` is expanded by Kubernetes from the secret, so the
password never appears on a command line. To restore to an earlier moment
than the last dump, use WAL-G's point-in-time recovery; the header of
[`deploy/k8s/base/infra/postgres.yaml`](../deploy/k8s/base/infra/postgres.yaml)
has the procedure.

### Upgrading and rolling back

Check out the new release, put its tag in the overlay's `images:` block (or
build it under a new tag with `scripts/build-images.sh … --push`), compare
the example overlay with yours for new settings, and apply. Take a dump first:
the gateway migrates the database when it starts, and migrations only go
forward. Rolling back the code is the previous tag; rolling back the data is
a restore.

Never deploy `latest`: a pod that restarts for any reason pulls whatever was
pushed last.

**Upgrading from 2026.10.07 or earlier.** Those releases also deployed twelve
gRPC services, NATS and Redis, which nothing used; the manifests no longer
have them, and `kubectl apply` does not delete what a manifest stopped
listing. Apply the new release first (it also moves the integration worker to
an HTTP health probe, which its new image needs), then delete them:

```bash
apps='ai-assistant,audit,calculation,identity,import,model,notification,policy,query,schema-migration,tenant,workflow,redis'
kubectl -n mavericks delete deployment,service,networkpolicy -l "app in ($apps)"
kubectl -n mavericks delete statefulset,service,networkpolicy -l app=nats
kubectl -n mavericks delete pvc nats-data-nats-0   # NATS held only transient events
kubectl -n mavericks get deploy,statefulset        # gateway, integration, web, keycloak, pgbouncer, postgres (and minio)
```

---

## Security checklist

- [ ] Only ports 80 and 443 are reachable from outside; SSH only from your own
      addresses.
- [ ] `.env` (Compose) or the secrets' values (Kubernetes) are in a password
      manager, and nowhere in git unless sealed.
- [ ] Backups go to storage outside the server or cluster, and a restore has
      been tested.
- [ ] Keycloak's `/admin` is restricted to your administrators' addresses, or
      at least the `admin` password is long and stored safely.
- [ ] The first administrator's password was changed from anything shared
      during setup, on Keycloak's account page:
      `https://auth.mavericks.example.com/realms/mavericks/account`.
- [ ] You know which revision you run, and you upgrade deliberately.
- [ ] Public sign-up stays off unless you mean to offer the platform to
      strangers — and then your terms and privacy notice are published.
