#!/usr/bin/env bash
# scripts/test-workflow-demo.sh — walk every step of the seeded Budget
# Approval workflow against a RUNNING dev stack, twice: once approving,
# once rejecting. Verifies at each step that the right persona sees the
# right task and that routing/skipping/instance status behave.
#
# Prerequisites:
#   make dev-up          # postgres etc.
#   make demo            # seed the OPEX Planning demo
#   bash dev.sh          # gateway on :8080 (DEV_MODE)
#
# Usage:
#   bash scripts/test-workflow-demo.sh
#
# Personas (X-Dev-User): dept_head = Alex (business_user, submitter)
#                        finance   = Jordan (business_admin, approver)
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
PSQL="docker exec docker-postgres-1 psql -U mavericks -d mavericks -t -A -c"

pass=0; fail=0
ok()   { pass=$((pass+1)); printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  \033[31m✗\033[0m %s\n' "$1"; }
check() { # check <description> <actual> <expected>
  if [ "$2" = "$3" ]; then ok "$1 [$2]"; else bad "$1 — got [$2], want [$3]"; fi
}
say() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# req runs in $(...) subshells, so the HTTP status is passed back through a
# temp file instead of a variable; read it with last_status.
STATUS_FILE=$(mktemp)
trap 'rm -f "$STATUS_FILE"' EXIT
req() { # req <persona> <method> <path> [json-body] → body on stdout
  local persona=$1 method=$2 path=$3 body=${4:-}
  local out
  out=$(curl -s -w $'\n%{http_code}' -X "$method" \
        -H "X-Dev-User: $persona" -H "X-App-Id: $APP_ID" \
        -H "Content-Type: application/json" \
        ${body:+-d "$body"} "$BASE$path")
  printf '%s' "${out##*$'\n'}" > "$STATUS_FILE"
  printf '%s' "${out%$'\n'*}"
}
last_status() { cat "$STATUS_FILE"; }

jsonget() { # jsonget <json> <python-expr over parsed j>
  python3 -c "import sys,json; j=json.loads(sys.argv[1]); print($2)" "$1" 2>/dev/null || echo ""
}

say "0. Preflight"
APP_ID=$($PSQL "SELECT id FROM core.application WHERE name='OPEX Planning 2026'")
[ -n "$APP_ID" ] && ok "app OPEX Planning 2026 found ($APP_ID)" || { bad "app not found — run 'make demo'"; exit 1; }
curl -sf "$BASE/api/dev/personas" >/dev/null && ok "gateway answering on $BASE" || { bad "gateway not running — run 'bash dev.sh'"; exit 1; }

say "1. Trigger is wired to a published workflow"
RULES=$(req developer GET /api/automation/rules)
RULE_ID=$(jsonget "$RULES" "j[0]['id']")
check "automation rule exists" "$(jsonget "$RULES" "j[0]['name']")" "budget_approval_trigger"
check "rule linked by workflow_def_id" "$(jsonget "$RULES" "bool(j[0].get('workflow_def_id'))")" "True"
check "workflow is published" "$($PSQL "SELECT status FROM workflow.workflow_def WHERE name='Budget Approval' ORDER BY created_at DESC LIMIT 1")" "published"

say "2. Business user can reach the start button on the dashboard"
DASHES=$(req dept_head GET /api/dashboards)
DASH_ID=$(jsonget "$DASHES" "[d['id'] for d in j if d['name']=='OPEX Planning'][0]")
[ -n "$DASH_ID" ] && ok "Alex (dept_head) sees the OPEX Planning dashboard" || bad "dashboard not visible to dept_head"
DETAIL=$(req dept_head GET "/api/dashboards/$DASH_ID")
check "dashboard carries the Submit for Approval button" \
  "$(jsonget "$DETAIL" "any(w.get('widget_type')=='automation_button' and w.get('ref_id')=='$RULE_ID' for w in j['widgets'])")" "True"
check "dashboard explains the approval process (text widget)" \
  "$(jsonget "$DETAIL" "any(w.get('widget_type')=='text' for w in j['widgets'])")" "True"

# ─────────────────────────────────────────────────────────────────────────────
run_path() { # run_path <decision> <expected-instance> <expected-notify> <label>
  local decision=$1 want_inst=$2 want_notify=$3 label=$4

  say "$label — Alex presses 'Submit for Approval'"
  EXEC=$(req dept_head POST "/api/automation/trigger/$RULE_ID" '{"payload":{}}')
  check "trigger returns 200" "$(last_status)" "200"
  INST_ID=$(jsonget "$EXEC" "j['instance_id']")
  [ -n "$INST_ID" ] && ok "workflow instance started ($INST_ID)" || bad "no instance_id in execution"

  say "$label — the approval step lands in the RIGHT inbox"
  TASKS_FIN=$(req finance GET /api/tasks)
  STEP_ID=$(jsonget "$TASKS_FIN" "[t['id'] for t in j if t['instance_id']=='$INST_ID' and t['step_name']=='Finance Review'][0]")
  [ -n "$STEP_ID" ] && ok "Jordan (finance/business_admin) sees 'Finance Review'" || bad "Finance Review missing from Jordan's inbox"
  check "step carries instructions for the approver" \
    "$(jsonget "$TASKS_FIN" "bool([t for t in j if t['id']=='$STEP_ID'][0]['instructions'])")" "True"
  check "Alex (submitter) does NOT see the approval step" \
    "$(jsonget "$(req dept_head GET /api/tasks)" "any(t.get('instance_id')=='$INST_ID' for t in j)")" "False"

  say "$label — wrong user & missing comment are refused"
  req dept_head POST "/api/tasks/$STEP_ID/complete" '{"decision":"'$decision'","comment":"sneaky"}' >/dev/null
  check "Alex completing Jordan's step → 403" "$(last_status)" "403"
  req finance POST "/api/tasks/$STEP_ID/complete" '{"decision":"'$decision'"}' >/dev/null
  check "no comment (required_comment) → 400" "$(last_status)" "400"

  say "$label — Jordan decides: $decision"
  req finance POST "/api/tasks/$STEP_ID/complete" '{"decision":"'$decision'","comment":"scripted retest"}' >/dev/null
  check "completion returns 200" "$(last_status)" "200"

  check "instance status" "$($PSQL "SELECT status FROM workflow.workflow_instance WHERE id='$INST_ID'")" "$want_inst"
  check "notify step" "$($PSQL "SELECT status||'/'||COALESCE(decision,'-') FROM workflow.workflow_step WHERE instance_id='$INST_ID' AND step_def_id='step-notify'")" "$want_notify"
  check "no tasks left in Jordan's inbox for this instance" \
    "$(jsonget "$(req finance GET /api/tasks)" "any(t.get('instance_id')=='$INST_ID' for t in j)")" "False"
}

run_path approve completed  "completed/sent" "3. APPROVE path"
run_path reject  cancelled  "skipped/-"      "4. REJECT path"

say "5. Execution log links trigger → instance"
check "every execution row reaches a real instance" \
  "$($PSQL "SELECT COUNT(*) FROM workflow.execution e WHERE e.application_id='$APP_ID' AND (e.instance_id IS NULL OR NOT EXISTS (SELECT 1 FROM workflow.workflow_instance wi WHERE wi.id=e.instance_id))")" "0"

say "──────────────────────────────"
printf '\033[1mResult: %d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
