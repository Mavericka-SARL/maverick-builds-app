#!/usr/bin/env bash
# daily-check.sh — one mail every morning saying the servers are watched
# (asked for on 2026-11-10, after the test of the disk alert).
#
# node-stats.sh mails only when something is wrong, so a quiet inbox meant
# either "all is well" or "the watcher, the relay or the mail path broke".
# This mail removes the doubt: it arrives every day with each node's latest
# sample (ops.node_stat), so a missing mail is itself the alarm, and a node
# whose watcher stopped sampling is named in the subject.
#
# It also names the tables a rewrite would shrink (pg-repack.sh --report:
# dead space of REPACK_MIN_MB and REPACK_MIN_SHARE of a table's files), with
# the command that rewrites them: the rewrite has no schedule (2026-11-11).
#
# Env vars:
#   DATABASE_URL            the control database (required)
#   SAMPLE_MAX_AGE_MINUTES  default: 15 (node-stats samples every 5)
#   DISK_ALERT_PERCENT      default: 85, as node-stats.sh
#   ALERT_WHERE             names the deployment in the subject, e.g. the
#                           namespace; empty for none
#   ALERT_EMAIL, SMTP_*     as alert-mail.sh
#   REPACK_*                as pg-repack.sh
#
# Exits 1 when a node is silent or past the disk line, or the samples
# cannot be read — after mailing.

set -uo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
SAMPLE_MAX_AGE_MINUTES="${SAMPLE_MAX_AGE_MINUTES:-15}"
DISK_ALERT_PERCENT="${DISK_ALERT_PERCENT:-85}"

log() { echo "daily-check: $*"; }
# shellcheck source=alert-mail.sh
. "$(dirname "$0")/alert-mail.sh"

where="${ALERT_WHERE:+ (${ALERT_WHERE})}"
footer="This mail comes every morning. If one does not arrive, the check, the
mail relay or the cluster is broken:
  kubectl get cronjob daily-check -A
An alert still comes on its own when a node's disk reaches ${DISK_ALERT_PERCENT}%
or its kernel kills a process for lack of memory."

# The server, a minute at most: a new pod's network policy can take a few
# seconds to admit it (pg-repack.sh, 2026-11-10).
for _ in $(seq 1 30); do
  psql -Atqc "SELECT 1" "$DATABASE_URL" >/dev/null 2>&1 && break
  sleep 2
done

# One line per node that sampled in the last 7 days (node-stats keeps 7):
# node, disk %, minutes since its latest sample, then the line for the body.
if ! rows="$(psql -Atq -F '|' -v ON_ERROR_STOP=1 "$DATABASE_URL" 2>/tmp/daily-check.out <<'SQL'
SELECT node, disk_pct, age_min,
       format('%-12s disk %s%% (%s of %s GB)   memory %s of %s MB free   load %s   %s',
              node, disk_pct, disk_used_gb, disk_total_gb, mem_available_mb, mem_total_mb, load1,
              CASE WHEN age_min < 1 THEN 'just now'
                   WHEN age_min < 120 THEN age_min || ' min ago'
                   ELSE (age_min / 60) || ' h ago' END)
FROM (
  SELECT DISTINCT ON (node) node, disk_pct, round(disk_used_gb, 1) AS disk_used_gb,
         round(disk_total_gb, 1) AS disk_total_gb, mem_available_mb, mem_total_mb, load1,
         floor(extract(epoch FROM now() - collected_at) / 60)::int AS age_min
  FROM ops.node_stat
  ORDER BY node, collected_at DESC
) latest
ORDER BY node
SQL
)"; then
  subject="[mavericks] daily check: cannot read the node samples${where}"
  log "${subject}: $(tr '\n' ' ' </tmp/daily-check.out)"
  send_alert_mail "$subject" "The daily check could not read ops.node_stat:

$(cat /tmp/daily-check.out)

${footer}"
  rm -f /tmp/daily-check.out
  exit 1
fi
rm -f /tmp/daily-check.out

if [ -z "$rows" ]; then
  subject="[mavericks] daily check: no node has reported for 7 days${where}"
  log "$subject"
  send_alert_mail "$subject" "ops.node_stat holds no sample: the node-stats DaemonSet is not running.

  kubectl get daemonset node-stats -A

${footer}"
  exit 1
fi

problems=""
disks=""
lines=""
while IFS='|' read -r node pct age line; do
  [ -n "$node" ] || continue
  disks="${disks:+${disks}, }${node} ${pct}%"
  lines="${lines}  ${line}
"
  if [ "$age" -gt "$SAMPLE_MAX_AGE_MINUTES" ]; then
    problems="${problems:+${problems}, }${node} silent"
  elif [ "$pct" -ge "$DISK_ALERT_PERCENT" ]; then
    problems="${problems:+${problems}, }${node} disk ${pct}%"
  fi
done <<EOF
$rows
EOF

# The tables a rewrite would shrink. Not a problem: PostgreSQL reuses the
# space for the table's next rows; the rewrite hands it back to the server.
if report="$("$(dirname "$0")/pg-repack.sh" --report 2>/tmp/daily-check.out)"; then
  if [ -n "$report" ]; then
    n=$(echo "$report" | wc -l | tr -d ' ')
    rewrite_note="; ${n} table(s) worth a rewrite"
    dead="Tables whose dead space a rewrite would give back (at least ${REPACK_MIN_MB:-64} MB, and ${REPACK_MIN_SHARE:-0.2} of their files):

$(echo "$report" | awk '{printf "  %-44s %-50s %6s MB dead of %s MB\n", $1, $2, $3, $4}')

The rewrite runs online (pg_repack: a moment's lock per table):
  kubectl -n ${ALERT_WHERE:-mavericks} create job --from=cronjob/postgres-repack repack-\$(date +%Y%m%d%H%M)"
  else
    rewrite_note=""
    dead="No table has dead space worth a rewrite."
  fi
else
  rewrite_note="; dead space not measured"
  dead="The dead space could not be measured: $(tr '\n' ' ' </tmp/daily-check.out)"
fi
rm -f /tmp/daily-check.out

if [ -n "$problems" ]; then
  subject="[mavericks] daily check: ${problems}${rewrite_note}${where}"
else
  subject="[mavericks] daily check: all well, disks ${disks}${rewrite_note}${where}"
fi
log "$subject"
send_alert_mail "$subject" "Each node's latest sample (node-stats, every 5 minutes):

${lines}
A node is silent when its latest sample is older than ${SAMPLE_MAX_AGE_MINUTES} minutes:
its node-stats pod is not running, or the node is down.

${dead}

${footer}"
[ -z "$problems" ]
