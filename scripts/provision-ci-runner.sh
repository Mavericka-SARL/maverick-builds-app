#!/usr/bin/env bash
#
# Provision a self-hosted GitHub Actions runner host.
#
# This existed only as a sequence of commands typed at worker-2 on 2026-08-19.
# Every piece of it was discovered by watching CI fail in a specific way, and
# none of that is recoverable from the machine itself — so rebuilding the box,
# or adding a second one, meant rediscovering the same four problems. The
# comments below are the point of the file as much as the commands are.
#
# Safe to re-run: every step checks before acting.
#
# Usage, as root on the target host:
#   REPO=owner/name RUNNERS=3 ./provision-ci-runner.sh
#
# A registration token is requested per runner. Generate them with:
#   gh api -X POST repos/OWNER/REPO/actions/runners/registration-token --jq .token
# and paste when prompted — they are short-lived and never stored here.
set -euo pipefail

REPO="${REPO:?set REPO=owner/name}"
RUNNERS="${RUNNERS:-3}"
RUNNER_VERSION="${RUNNER_VERSION:-2.336.0}"
RUNNER_USER="${RUNNER_USER:-runner}"
LABELS="${LABELS:-mavericks-ci}"

log() { printf '\n=== %s\n' "$*"; }

# ── The node may be running production ──────────────────────────────────────
# worker-2 hosts 15 live pods. Everything below is shaped by that: nothing here
# may disturb the pod network or starve the workloads already on the box.
log "Snapshotting iptables before touching anything"
mkdir -p /root/ci-runner-backup
iptables-save  > /root/ci-runner-backup/iptables-before.rules
ip6tables-save > /root/ci-runner-backup/ip6tables-before.rules 2>/dev/null || true
FORWARD_BEFORE=$(iptables -L FORWARD -n | head -1)
echo "$FORWARD_BEFORE"

log "Creating the $RUNNER_USER user"
id "$RUNNER_USER" >/dev/null 2>&1 || useradd -m -s /bin/bash "$RUNNER_USER"
# Subordinate id ranges are what rootless Docker maps container users into.
grep -q "^$RUNNER_USER:" /etc/subuid || echo "$RUNNER_USER:100000:65536" >> /etc/subuid
grep -q "^$RUNNER_USER:" /etc/subgid || echo "$RUNNER_USER:100000:65536" >> /etc/subgid
# Lingering keeps the user's systemd instance alive with nobody logged in,
# which is what runs the rootless Docker daemon.
loginctl enable-linger "$RUNNER_USER"
RUNNER_UID=$(id -u "$RUNNER_USER")

export DEBIAN_FRONTEND=noninteractive
log "Installing prerequisites"
apt-get update -qq
# build-essential is not optional: without a C compiler cgo is disabled, and
# `go test -race` refuses to build at all ("-race requires cgo").
apt-get install -y -qq uidmap dbus-user-session jq ca-certificates curl git build-essential >/dev/null

# ── Docker, rootless on purpose ─────────────────────────────────────────────
# The rootful daemon sets the iptables FORWARD policy to DROP when it starts.
# On a node running pod workloads that policy lands in front of the pod
# network. Masking the service before the packages arrive stops apt starting
# it even once.
log "Installing Docker with the rootful daemon masked"
systemctl mask docker.service docker.socket >/dev/null 2>&1 || true
if [ ! -f /etc/apt/keyrings/docker.asc ]; then
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
fi
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update -qq
apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-ce-rootless-extras >/dev/null

log "Verifying the FORWARD policy is untouched"
FORWARD_AFTER=$(iptables -L FORWARD -n | head -1)
if [ "$FORWARD_BEFORE" != "$FORWARD_AFTER" ]; then
  echo "ABORTING: iptables FORWARD policy changed from '$FORWARD_BEFORE' to '$FORWARD_AFTER'." >&2
  echo "Restore with: iptables-restore < /root/ci-runner-backup/iptables-before.rules" >&2
  exit 1
fi
echo "$FORWARD_AFTER"

log "Starting rootless Docker for $RUNNER_USER"
RU="XDG_RUNTIME_DIR=/run/user/$RUNNER_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$RUNNER_UID/bus PATH=/usr/bin:/sbin:/usr/sbin:/bin"
sudo -u "$RUNNER_USER" env $RU dockerd-rootless-setuptool.sh install >/dev/null 2>&1 || true
sudo -u "$RUNNER_USER" env $RU systemctl --user enable --now docker >/dev/null 2>&1 || true

# ── The port-range trap ─────────────────────────────────────────────────────
# Rootless Docker picks a published port inside dockerd's own network
# namespace, where host bindings are invisible, and RootlessKit then binds it
# on the host. Both default to 32768-60999, so a port Docker believes is free
# can already be held by an outbound socket from a test process: container
# starts fail with "bind: address already in use" on lines that have nothing to
# do with what is being tested. Confining host clients to a disjoint range
# removes the overlap. A fresh netns takes the kernel default rather than
# inheriting this, so Docker keeps 32768+ and the fix survives a restart.
# 30000-32767 is left alone: that is the Kubernetes NodePort range.
log "Separating host client ports from Docker's published-port range"
cat > /etc/sysctl.d/99-ci-rootless-ports.conf <<'EOF'
net.ipv4.ip_local_port_range = 15000 29999
EOF
sysctl -p /etc/sysctl.d/99-ci-rootless-ports.conf

# ── Resource ceiling ────────────────────────────────────────────────────────
# One slice for every runner together. Per-service quotas over-commit: three
# runners at 400% each promises twelve cores on an eight-core box. The low
# weights matter more than the caps — they make production win contention
# rather than merely being limited alongside CI.
log "Creating the bounded ci.slice"
cat > /etc/systemd/system/ci.slice <<'EOF'
[Unit]
Description=CI runners (bounded so they cannot starve production pods)
Before=slices.target

[Slice]
CPUQuota=500%
CPUWeight=20
MemoryMax=9G
MemoryHigh=7G
IOWeight=20
EOF
systemctl daemon-reload

# ── Runners ─────────────────────────────────────────────────────────────────
log "Installing $RUNNERS runner(s)"
for n in $(seq 1 "$RUNNERS"); do
  if [ "$n" = "1" ]; then DIR="/home/$RUNNER_USER/actions-runner"; NAME="$(hostname)"
  else DIR="/home/$RUNNER_USER/actions-runner-$n"; NAME="$(hostname)-$n"; fi

  if [ ! -f "$DIR/config.sh" ]; then
    sudo -u "$RUNNER_USER" mkdir -p "$DIR"
    sudo -u "$RUNNER_USER" bash -c "cd '$DIR' && curl -fsSL -o runner.tar.gz \
      https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz \
      && tar xzf runner.tar.gz && rm runner.tar.gz"
  fi
  # A copied runner directory carries .runner_migrated, which the agent reads
  # as "already configured" and refuses to register over.
  rm -f "$DIR/.runner_migrated"

  # HOME must be pinned: without it "~" resolves to the runner's INSTALL
  # directory for actions/cache but to the user's home for everything else, so
  # a restored cache lands where nothing will look for it. DOCKER_CONFIG and
  # BUILDX_CONFIG must NOT be shared: all runners drive one Docker daemon, and
  # with one ~/.docker/buildx between them a finishing job's cleanup removes a
  # buildkit container another job is still building against ("graceful_stop").
  sudo -u "$RUNNER_USER" tee "$DIR/.env" >/dev/null <<EOF
HOME=/home/$RUNNER_USER
DOCKER_HOST=unix:///run/user/$RUNNER_UID/docker.sock
XDG_RUNTIME_DIR=/run/user/$RUNNER_UID
DOCKER_CONFIG=/home/$RUNNER_USER/.docker-r$n
BUILDX_CONFIG=/home/$RUNNER_USER/.buildx-r$n
PATH=/usr/bin:/usr/local/bin:/bin:/usr/sbin:/sbin
EOF
  sudo -u "$RUNNER_USER" mkdir -p "/home/$RUNNER_USER/.docker-r$n" "/home/$RUNNER_USER/.buildx-r$n"

  if [ ! -f "$DIR/.runner" ]; then
    read -r -s -p "Registration token for $NAME: " TOKEN; echo
    sudo -u "$RUNNER_USER" bash -c "cd '$DIR' && ./config.sh --unattended --replace \
      --url 'https://github.com/$REPO' --token '$TOKEN' --name '$NAME' --labels '$LABELS' --work _work" >/dev/null
    unset TOKEN
  fi

  ( cd "$DIR" && ./svc.sh install "$RUNNER_USER" >/dev/null 2>&1 || true )
  SVC="/etc/systemd/system/actions.runner.$(echo "$REPO" | tr '/' '-').$NAME.service"
  mkdir -p "${SVC}.d"
  printf '[Service]\nSlice=ci.slice\n' > "${SVC}.d/slice.conf"
  systemctl daemon-reload
  ( cd "$DIR" && ./svc.sh start >/dev/null 2>&1 || true )
done

# ── Toolchain the hosted images ship and this box does not ──────────────────
# playwright install --with-deps shells out to `sudo apt-get`, which this user
# deliberately cannot do, so the system libraries are installed here instead
# and CI only ever downloads browsers.
log "Installing Playwright system libraries (best effort)"
NODEBIN=$(ls -d /home/"$RUNNER_USER"/actions-runner*/_work/_tool/node/*/x64/bin 2>/dev/null | head -1 || true)
if [ -n "$NODEBIN" ]; then
  PATH="$NODEBIN:$PATH" npx --yes playwright install-deps chromium >/dev/null 2>&1 \
    && echo "installed" || echo "skipped (run again after the first e2e job)"
else
  echo "skipped — no Node in the tool cache yet; re-run this script after the first e2e job"
fi

log "Done"
systemctl list-units 'actions.runner.*' --no-legend --plain | awk '{print "  " $1 "  " $4}'
echo
echo "Point CI at these runners by setting the RUNNER_LABEL repository variable to '$LABELS'."
echo "Clearing that variable sends every job back to GitHub-hosted runners with no code change."
