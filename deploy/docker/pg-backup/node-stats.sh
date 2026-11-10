#!/usr/bin/env bash
# node-stats.sh — runs once per node (deploy/k8s/base/infra/node-stats.yaml).
# Every CHECK_EVERY_SECONDS it records the node's root disk, memory and load
# in ops.node_stat (the admin consoles' Infrastructure screen), and mails
# ALERT_EMAIL when
#   - the node's root filesystem is DISK_ALERT_PERCENT full or more. Every
#     pod on the node shares it: the kubelet deletes images from 85% and
#     evicts pods once less than 5% is free, so a disk filled by anything on
#     the node (build jobs, logs, images) takes the application's pods with
#     it, and otherwise shows only when someone looks;
#   - the kernel killed a process for lack of memory (oom_kill in
#     /proc/vmstat rose). A container past its memory limit is killed the
#     same way: a gateway killed mid-calculation loses every request in
#     flight, and nothing else says so.
#
# Env vars:
#   NODE_NAME, DATABASE_URL   required (the DaemonSet sets both)
#   HOST_ROOT                 default: /host, the node's / mounted read-only
#   HOST_PROC                 default: /host_proc, the node's /proc
#   DISK_ALERT_PERCENT        default: 85
#   CHECK_EVERY_SECONDS       default: 300
#   REMIND_EVERY_HOURS        default: 24. Mail again while the disk stays full.
#   OOM_KILLS_SEEN            the oom_kill count already reported; default:
#                             the count at start, so a restart does not
#                             report kills from before it
#   ALERT_WHERE               names the deployment in the subject, e.g. the
#                             namespace; empty for none.
#   ALERT_EMAIL, SMTP_*       as backup-watchdog.sh (alert-mail.sh).
#   ONCE                      "1": one check, then exit.
#
# Mails when the disk crosses the line, every REMIND_EVERY_HOURS while it
# stays there, and once when it is back under; one mail per check that saw
# OOM kills.

set -uo pipefail

: "${NODE_NAME:?NODE_NAME is required}"
: "${DATABASE_URL:?DATABASE_URL is required}"
HOST_ROOT="${HOST_ROOT:-/host}"
HOST_PROC="${HOST_PROC:-/host_proc}"
DISK_ALERT_PERCENT="${DISK_ALERT_PERCENT:-85}"
CHECK_EVERY_SECONDS="${CHECK_EVERY_SECONDS:-300}"
REMIND_EVERY_HOURS="${REMIND_EVERY_HOURS:-24}"
ONCE="${ONCE:-}"

log() { echo "node-stats: $*"; }
# shellcheck source=alert-mail.sh
. "$(dirname "$0")/alert-mail.sh"

where=" (${ALERT_WHERE:+${ALERT_WHERE}, }node ${NODE_NAME})"
oom_kills() { awk '$1 == "oom_kill" { print $2 }' "$HOST_PROC/vmstat" 2>/dev/null; }

oom_seen="${OOM_KILLS_SEEN:-$(oom_kills)}"
disk_alerted_at=0

while true; do
  line=$(df -k "$HOST_ROOT" | tail -1)
  total_kb=$(echo "$line" | awk '{print $2}')
  used_kb=$(echo "$line" | awk '{print $3}')
  pct=$(echo "$line" | awk '{print $5}' | tr -dc '0-9')
  total_gb=$(awk "BEGIN{printf \"%.1f\", $total_kb/1048576}")
  used_gb=$(awk "BEGIN{printf \"%.1f\", $used_kb/1048576}")
  mem_total=$(awk '/MemTotal/{printf "%d", $2/1024}' "$HOST_PROC/meminfo")
  mem_avail=$(awk '/MemAvailable/{printf "%d", $2/1024}' "$HOST_PROC/meminfo")
  load1=$(awk '{print $1}' "$HOST_PROC/loadavg")
  psql "$DATABASE_URL" -q -c "INSERT INTO ops.node_stat (node, disk_total_gb, disk_used_gb, disk_pct, mem_total_mb, mem_available_mb, load1) VALUES ('$NODE_NAME', $total_gb, $used_gb, $pct, $mem_total, $mem_avail, $load1); DELETE FROM ops.node_stat WHERE collected_at < now() - interval '7 days';" \
    || log "could not record the sample"

  now=$(date +%s)
  if [ "$pct" -ge "$DISK_ALERT_PERCENT" ]; then
    if [ "$disk_alerted_at" = 0 ] || [ $((now - disk_alerted_at)) -ge $((REMIND_EVERY_HOURS * 3600)) ]; then
      subject="[mavericks] node disk ${pct}% full${where}"
      log "ALERT: ${subject}"
      send_alert_mail "$subject" "The root filesystem of node ${NODE_NAME} is ${pct}% full (${used_gb} of ${total_gb} GB).

The kubelet deletes unused images from 85% and evicts this node's pods once
less than 5% is free, whatever filled it.

  df -h /                       (on the node)
  kubectl get events -A --field-selector reason=Evicted
  kubectl describe node ${NODE_NAME} | grep -A8 Conditions"
      disk_alerted_at=$now
    fi
  elif [ "$disk_alerted_at" != 0 ]; then
    subject="[mavericks] node disk back to ${pct}%${where}"
    log "${subject}"
    send_alert_mail "$subject" "The root filesystem of node ${NODE_NAME} is ${pct}% full (${used_gb} of ${total_gb} GB), under ${DISK_ALERT_PERCENT}%."
    disk_alerted_at=0
  fi

  kills="$(oom_kills)"
  if [ -n "$kills" ] && [ -n "$oom_seen" ] && [ "$kills" -gt "$oom_seen" ]; then
    subject="[mavericks] $((kills - oom_seen)) process(es) killed for lack of memory${where}"
    log "ALERT: ${subject}"
    send_alert_mail "$subject" "The kernel of node ${NODE_NAME} killed $((kills - oom_seen)) process(es) for lack of memory
since the last check (${CHECK_EVERY_SECONDS} s); ${mem_avail} of ${mem_total} MB available now.

A container past its own memory limit is killed the same way, and restarts.

  kubectl get pods -A -o wide | grep ${NODE_NAME}       (RESTARTS, and OOMKilled as the last state)
  kubectl get events -A --field-selector involvedObject.name=${NODE_NAME}"
  fi
  [ -n "$kills" ] && oom_seen="$kills"

  [ -n "$ONCE" ] && break
  sleep "$CHECK_EVERY_SECONDS"
done
