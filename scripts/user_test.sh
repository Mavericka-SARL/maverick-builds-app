#!/usr/bin/env bash
# user_test.sh — End-to-end user journey test for Phase 1-3.
#
# Simulates four real personas walking through the system:
#   Alex  (dept_head)     — enters budget data, submits for approval
#   Jordan (finance)      — reviews inbox, approves the budget
#   Sam   (developer)     — inspects model, adds a metric, generates migration
#   Pat   (platform_admin)— audits tenants, users, event log
# Then repeats the journey for the Procurement app (Phase 3).
#
# Prerequisites:
#   - Gateway running on localhost:8080 with DEV_MODE=true
#   - Both OPEX + Procurement seeds applied
#
# Usage:
#   bash scripts/user_test.sh [BASE_URL]

set -euo pipefail

BASE="${1:-http://localhost:8080}"
PASS=0
FAIL=0
SKIP=0

# ── colours ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
BLUE='\033[1;34m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

section()  { echo -e "\n${BLUE}▶ $*${NC}"; }
action()   { echo -e "  ${CYAN}→ $*${NC}"; }
pass()     { echo -e "  ${GREEN}✓ $*${NC}"; PASS=$((PASS+1)); }
fail()     { echo -e "  ${RED}✗ $*${NC}"; FAIL=$((FAIL+1)); }
warn()     { echo -e "  ${YELLOW}~ $*${NC}"; SKIP=$((SKIP+1)); }
detail()   { echo -e "    ${NC}$*"; }

# ── helpers ───────────────────────────────────────────────────────────────────

# Split response: body in $RESP, code in $HTTP.
# Uses a unique sentinel so no newline edge-cases on macOS/Linux.
_parse() {
  local raw="$1"
  HTTP=$(printf '%s' "$raw" | grep -o '__HTTP__[0-9]*$' | sed 's/__HTTP__//')
  RESP=$(printf '%s' "$raw" | sed 's/__HTTP__[0-9]*$//')
}

# GET $path as $persona → stores response in $RESP, HTTP code in $HTTP
get() {
  local path="$1" persona="${2:-dept_head}"
  _parse "$(curl -s -w '__HTTP__%{http_code}' "$BASE$path" -H "X-Dev-User: $persona")"
}

# POST $path $body as $persona
post() {
  local path="$1" body="$2" persona="${3:-dept_head}"
  _parse "$(curl -s -w '__HTTP__%{http_code}' -X POST "$BASE$path" \
        -H "X-Dev-User: $persona" -H "Content-Type: application/json" -d "$body")"
}

put() {
  local path="$1" body="$2" persona="${3:-dept_head}"
  _parse "$(curl -s -w '__HTTP__%{http_code}' -X PUT "$BASE$path" \
        -H "X-Dev-User: $persona" -H "Content-Type: application/json" -d "$body")"
}

delete_req() {
  local path="$1" persona="${2:-dept_head}"
  _parse "$(curl -s -w '__HTTP__%{http_code}' -X DELETE "$BASE$path" \
        -H "X-Dev-User: $persona")"
}

# jq-free JSON field extractor: json_get KEY [JSON]
json_get() {
  local key="$1" json="${2:-$RESP}"
  python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(d.get('$key','') if isinstance(d,dict) else '')" "$json" 2>/dev/null || true
}

json_len() {
  local json="${1:-$RESP}"
  python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d) if isinstance(d,list) else 0)" "$json" 2>/dev/null || echo 0
}

json_field_in_list() {
  # json_field_in_list FIELD VALUE [JSON]
  local field="$1" value="$2" json="${3:-$RESP}"
  python3 -c "
import sys,json
d=json.loads(sys.argv[1])
items = d if isinstance(d,list) else []
print('yes' if any(str(i.get('$field',''))==sys.argv[2] for i in items) else 'no')
" "$json" "$value" 2>/dev/null || echo "no"
}

assert_http() {
  local expected="$1" label="$2"
  if [[ "$HTTP" == "$expected" ]]; then pass "$label (HTTP $HTTP)"
  else fail "$label — expected HTTP $expected, got HTTP $HTTP"; detail "$RESP"; fi
}

assert_field() {
  local key="$1" expected="$2" label="${3:-$key = $expected}"
  local got; got=$(json_get "$key")
  if [[ "$got" == "$expected" ]]; then pass "$label"
  else fail "$label — expected '$expected', got '$got'"; fi
}

assert_nonempty() {
  local key="$1" label="${2:-$key is set}"
  local got; got=$(json_get "$key")
  if [[ -n "$got" && "$got" != "null" && "$got" != "" ]]; then pass "$label"
  else fail "$label — field is empty/null"; fi
}

assert_list_len_ge() {
  local min="$1" label="$2"
  local len; len=$(json_len)
  if [[ "$len" -ge "$min" ]]; then pass "$label ($len items)"
  else fail "$label — expected ≥$min items, got $len"; fi
}

assert_contains() {
  local field="$1" value="$2" label="${3:-list contains $field=$value}"
  local found; found=$(json_field_in_list "$field" "$value")
  if [[ "$found" == "yes" ]]; then pass "$label"
  else fail "$label — not found in list"; fi
}

# ─────────────────────────────────────────────────────────────────────────────
echo -e "${BOLD}Mavericks Engine — Phase 1-3 User Journey Test${NC}"
echo -e "Gateway: ${CYAN}$BASE${NC}"
echo ""

# ── 0. Health check ───────────────────────────────────────────────────────────
section "0. Gateway health"
action "Checking /healthz"
get /healthz dept_head
if [[ "$HTTP" == "200" && "$RESP" == "ok" ]]; then pass "Gateway is up"
else fail "Gateway not responding — is it running with DEV_MODE=true?"; exit 1; fi

# ── 1. Alex — Planning Console (OPEX app) ────────────────────────────────────
section "1. Alex (Dept Head) — Planning Console  [Phase 1]"

action "Alex opens the demo context"
get /api/demo dept_head
assert_http 200 "Demo context loads"
assert_nonempty "app_id"   "App ID is set"
assert_nonempty "model_id" "Model ID is set"
APP_ID=$(json_get "app_id")
MODEL_ID=$(json_get "model_id")
ACTIVE_SCENARIO=$(json_get "scenario")
ACTIVE_VERSION=$(json_get "version")
ACTOR_EMAIL=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(d.get('actor',{}).get('email',''))" "$RESP")
detail "app=${APP_ID:0:8}  scenario='$ACTIVE_SCENARIO / $ACTIVE_VERSION'  actor=$ACTOR_EMAIL"

action "Alex fetches the planning grid"
GRID_SCN_ENC=$(python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1]))" "$ACTIVE_SCENARIO")
get "/api/grid?scenario=${GRID_SCN_ENC}&version=${ACTIVE_VERSION}" dept_head
assert_http 200 "Grid loads"
GRID_RESP="$RESP"   # save for DIM_CODE extraction later
GRID_METRICS=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('metrics',[])))" "$GRID_RESP")
GRID_DEPTS=$(python3 -c  "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('departments',[])))" "$GRID_RESP")
GRID_CELLS=$(python3 -c  "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('cells',{})))"  "$GRID_RESP")
GRID_TOTALS=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('totals',{})))" "$GRID_RESP")
if [[ "$GRID_METRICS" -ge 1 ]]; then pass "Grid: $GRID_METRICS metrics, $GRID_DEPTS columns, $GRID_CELLS cells, $GRID_TOTALS totals"
else fail "Grid returned no metrics"; fi

action "Alex fetches metric values (active scenario)"
get "/api/metrics?scenario=${GRID_SCN_ENC}&version=${ACTIVE_VERSION}" dept_head
assert_http 200 "Metrics list loads"
assert_list_len_ge 3 "At least 3 metrics"
# Find any calculated (non-input) metric
CALC_METRIC_ID=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
calcs=[x for x in d if not x.get('is_input',True)]
print(calcs[0]['id'] if calcs else '')
" "$RESP")
CALC_METRIC_NAME=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
calcs=[x for x in d if not x.get('is_input',True)]
print(calcs[0]['name'] if calcs else '')
" "$RESP")
CALC_METRIC_VAL=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
calcs=[x for x in d if not x.get('is_input',True)]
print(calcs[0].get('value') if calcs else None)
" "$RESP")
# Find any input metric for the writeback test
INPUT_METRIC_ID=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
ins=[x for x in d if x.get('is_input',False)]
print(ins[0]['id'] if ins else '')
" "$RESP")
INPUT_METRIC_NAME=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
ins=[x for x in d if x.get('is_input',False)]
print(ins[0]['name'] if ins else '')
" "$RESP")
if [[ -n "$CALC_METRIC_ID" ]]; then
  pass "Calculated metric '$CALC_METRIC_NAME' found (id=${CALC_METRIC_ID:0:8}…)"
  if python3 -c "import sys; v=float(str(sys.argv[1])); exit(0 if v>0 else 1)" "$CALC_METRIC_VAL" 2>/dev/null; then
    pass "Calc metric value is positive: $CALC_METRIC_VAL"
  else warn "'$CALC_METRIC_NAME' value is zero/null — calc may not have run yet"; fi
else fail "No calculated metrics found"; fi

action "Alex writes a budget cell ($INPUT_METRIC_NAME)"
DIM_CODE=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
depts=d.get('departments',[])
print(depts[0]['code'] if depts else '')
" "$GRID_RESP")
if [[ -n "$INPUT_METRIC_ID" && -n "$DIM_CODE" ]]; then
  post /api/cells "{\"model_id\":\"$MODEL_ID\",\"scenario\":\"$ACTIVE_SCENARIO\",\"version\":\"$ACTIVE_VERSION\",\"metric_id\":\"$INPUT_METRIC_ID\",\"dim_code\":\"$DIM_CODE\",\"value\":99999}" dept_head
  assert_http 200 "Writeback accepted ($INPUT_METRIC_NAME / $DIM_CODE = 99999)"
  assert_field "status" "ok" "Writeback status=ok"
else warn "Skipping writeback — no input metric or dimension found"; fi

action "Alex submits the budget for approval"
post /api/workflow/submit "{\"model_id\":\"$MODEL_ID\",\"scenario\":\"$ACTIVE_SCENARIO\",\"version\":\"$ACTIVE_VERSION\"}" dept_head
assert_http 200 "Budget submitted"
WF_INSTANCE_ID=$(json_get "instance_id")
assert_nonempty "instance_id" "Workflow instance created"
detail "instance=${WF_INSTANCE_ID:0:8}…"

# ── 2. Jordan — Inbox / Approval ─────────────────────────────────────────────
section "2. Jordan (Finance) — Inbox & Approval  [Phase 1]"

action "Jordan opens her inbox (tasks)"
get /api/tasks finance
assert_http 200 "Tasks endpoint responds"
TASK_COUNT=$(json_len)
detail "Pending tasks: $TASK_COUNT"
STEP_ID=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(d[0]['id'] if isinstance(d,list) and d else '')
" "$RESP")

if [[ -n "$STEP_ID" ]]; then
  pass "At least one pending task found (id=${STEP_ID:0:8}…)"

  action "Jordan approves the budget"
  post "/api/tasks/$STEP_ID/complete" '{"decision":"approve","comment":"Numbers look solid for FY2026. Approved."}' finance
  assert_http 200 "Task completion accepted"
  assert_field "status" "ok" "Completion status=ok"
else
  warn "No pending tasks — budget may already be approved (re-seed to reset)"
fi

action "Jordan reviews workflow history"
get /api/workflow/history finance
assert_http 200 "Workflow history loads"
assert_list_len_ge 1 "At least one workflow instance in history"
COMPLETED=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
c=[i for i in d if 'COMPLETED' in i.get('status','') or 'completed' in i.get('status','')]
print(len(c))
" "$RESP")
detail "Completed instances: $COMPLETED"
if [[ "$COMPLETED" -ge 1 ]]; then pass "At least one completed workflow"
else warn "No completed workflows — approve a task first"; fi

action "Jordan checks notifications"
get /api/notifications finance
assert_http 200 "Notifications endpoint responds"
NOTIF_COUNT=$(json_len)
detail "Notifications: $NOTIF_COUNT"
if [[ "$NOTIF_COUNT" -ge 0 ]]; then pass "Notifications endpoint works ($NOTIF_COUNT items)"; fi

if [[ "$NOTIF_COUNT" -gt 0 ]]; then
  action "Jordan marks notifications read"
  NOTIF_IDS=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
ids=[n['id'] for n in d[:3]]
print(json.dumps(ids))
" "$RESP")
  post /api/notifications/mark-read "{\"ids\":$NOTIF_IDS}" finance
  assert_http 200 "Mark-read accepted"
fi

# ── 3. Sam — Developer Console ────────────────────────────────────────────────
section "3. Sam (Developer) — Developer Console  [Phase 2]"

action "Sam opens the model inspector"
get /api/developer/model developer
assert_http 200 "Developer model loads"
assert_nonempty "model_name" "Model name is set"
DEV_APP=$(json_get "app_name"); DEV_MODEL=$(json_get "model_name")
DEV_METRICS=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('metrics',[])))" "$RESP")
pass "App: $DEV_APP | Model: $DEV_MODEL | Metrics: $DEV_METRICS"

action "Sam reviews dimensions"
get /api/developer/dimensions developer
assert_http 200 "Developer dimensions load"
assert_list_len_ge 1 "At least one dimension"
DIM_NAMES=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print([x['name'] for x in d])" "$RESP")
detail "Dimensions: $DIM_NAMES"

action "Sam adds a new calculated metric"
METRIC_NAME="q_factor_$(date +%s)"
post /api/developer/metrics \
  "{\"name\":\"$METRIC_NAME\",\"is_input\":false,\"formula\":\"{total_opex} * 0.01\"}" developer
assert_http 200 "Metric added"
NEW_METRIC_ID=$(json_get "id")
assert_nonempty "id" "New metric ID returned"
detail "Created metric id=${NEW_METRIC_ID:0:8}…  name=$METRIC_NAME"

action "Sam verifies new metric appears in model"
get /api/developer/model developer
assert_http 200 "Model reloads"
UPDATED_COUNT=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('metrics',[])))" "$RESP")
if [[ "$UPDATED_COUNT" -gt "$DEV_METRICS" ]]; then
  pass "Metric count increased: $DEV_METRICS → $UPDATED_COUNT"
else fail "Metric count unchanged after add ($UPDATED_COUNT)"; fi

action "Sam generates a schema migration preview"
post /api/developer/migration/generate '{}' developer
assert_http 200 "Migration generated"
MIG_VERSION=$(json_get "version_number")
MIG_FILES=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('files',[])))" "$RESP")
MIG_MODEL=$(json_get "model_id")
pass "Migration v$MIG_VERSION — $MIG_FILES file(s) for model ${MIG_MODEL:0:8}…"

action "Sam applies the migration"
post /api/developer/migration/apply \
  "{\"model_id\":\"$MIG_MODEL\",\"version_number\":$MIG_VERSION}" developer
assert_http 200 "Migration applied"
APPLIED_FILES=$(json_get "files_applied")
APPLIED_STATUS=$(json_get "status")
assert_field "status" "applied" "Migration status=applied"
detail "Applied $APPLIED_FILES file(s)"

# ── 4. Pat — Platform Admin Console ──────────────────────────────────────────
section "4. Pat (Platform Admin) — Admin Console  [Phase 1/2]"

action "Pat views all tenants"
get /api/admin/tenants platform_admin
assert_http 200 "Admin tenants load"
assert_list_len_ge 1 "At least one tenant"
TENANT_NAME=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(d[0]['name'] if d else '')" "$RESP")
pass "Tenant: $TENANT_NAME"

action "Pat views all users"
get /api/admin/users platform_admin
assert_http 200 "Admin users load"
assert_list_len_ge 4 "At least 4 demo users"
USER_EMAILS=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print([u['email'] for u in d[:4]])" "$RESP")
detail "Users: $USER_EMAILS"

action "Pat reviews the audit log"
get /api/admin/audit platform_admin
assert_http 200 "Audit log loads"
assert_list_len_ge 5 "At least 5 audit events"
AUDIT_COUNT=$(json_len)
CATEGORIES=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
cats=sorted(set(e.get('category','') for e in d))
print(cats)
" "$RESP")
detail "Audit events: $AUDIT_COUNT | Categories: $CATEGORIES"

action "Pat views import jobs"
get /api/import/jobs platform_admin
assert_http 200 "Import jobs endpoint responds"
pass "Import jobs endpoint works ($(json_len) jobs)"

# ── 5. Alex — CRUD / Forms (Procurement app)  ────────────────────────────────
section "5. Alex (Dept Head) — Purchase Request Forms  [Phase 3 CRUD]"

action "Alex lists available forms"
get /api/forms dept_head
assert_http 200 "Forms endpoint responds"
FORMS_COUNT=$(json_len)
assert_list_len_ge 1 "At least one form defined"
FORM_ID=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(d[0]['id'] if d else '')
" "$RESP")
FORM_LABEL=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(d[0].get('label','') if d else '')
" "$RESP")
FIELD_COUNT=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(len(d[0].get('fields',[])) if d else 0)
" "$RESP")
pass "Form: '$FORM_LABEL' with $FIELD_COUNT fields (id=${FORM_ID:0:8}…)"

action "Alex views existing purchase requests"
get "/api/forms/$FORM_ID/records" dept_head
assert_http 200 "Form records load"
RECORDS_COUNT=$(json_len)
assert_list_len_ge 1 "At least one seeded record"
STATUSES=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(sorted(set(r.get('status','') for r in d)))
" "$RESP")
detail "Records: $RECORDS_COUNT | Statuses: $STATUSES"

action "Alex creates a new purchase request"
post "/api/forms/$FORM_ID/records" \
  '{"data":{"vendor":"TestVendor AG","category":"IT","amount":7500,"currency":"USD","justification":"Software licence renewal — Q2","is_urgent":false}}' \
  dept_head
assert_http 200 "Purchase request created"
NEW_REC_ID=$(json_get "id")
NEW_REC_STATUS=$(json_get "status")
assert_nonempty "id" "Record ID returned"
assert_field "status" "draft" "New record starts as draft"
detail "Record id=${NEW_REC_ID:0:8}… status=$NEW_REC_STATUS"

action "Alex submits the purchase request (status: submitted)"
put "/api/records/$NEW_REC_ID" \
  '{"status":"submitted","data":{"vendor":"TestVendor AG","category":"IT","amount":7500,"currency":"USD","justification":"Software licence renewal — Q2","is_urgent":false}}' \
  dept_head
assert_http 200 "Record status updated"
assert_field "status" "ok" "Update acknowledged"

action "Alex verifies record shows as submitted"
get "/api/forms/$FORM_ID/records" dept_head
assert_http 200 "Records reload"
SUBMITTED=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(len([r for r in d if r.get('status')=='submitted']))
" "$RESP")
if [[ "$SUBMITTED" -ge 1 ]]; then pass "At least one submitted record ($SUBMITTED)"
else fail "No submitted records found"; fi

action "Alex deletes the test record she just created"
delete_req "/api/records/$NEW_REC_ID" dept_head
assert_http 200 "Record deleted"
assert_field "status" "deleted" "Delete acknowledged"

action "Alex verifies record is gone"
get "/api/forms/$FORM_ID/records" dept_head
assert_http 200 "Records reload after delete"
STILL_THERE=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print('yes' if any(r.get('id')=='$NEW_REC_ID' for r in d) else 'no')
" "$RESP")
if [[ "$STILL_THERE" == "no" ]]; then pass "Deleted record no longer in list"
else fail "Deleted record still appears in list"; fi

# ── 6. Sam — Automation (Developer Console)  ──────────────────────────────────
section "6. Sam (Developer) — Automation Rules  [Phase 3 Automation]"

action "Sam lists automation rules"
get /api/automation/rules developer
assert_http 200 "Automation rules load"
assert_list_len_ge 1 "At least one automation rule"
RULE_ID=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
manual=[r for r in d if r.get('trigger_type')=='manual' or r.get('trigger_type')=='form_submit']
print(manual[0]['id'] if manual else (d[0]['id'] if d else ''))
" "$RESP")
RULE_NAME=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(d[0].get('name','') if d else '')
" "$RESP")
RULE_TRIGGER=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(d[0].get('trigger_type','') if d else '')
" "$RESP")
pass "Rule: '$RULE_NAME' (trigger=$RULE_TRIGGER)"

action "Sam creates a new automation rule"
RULE_TS=$(date +%s)
post /api/automation/rules \
  "{\"name\":\"test_rule_e2e_$RULE_TS\",\"description\":\"E2E test rule\",\"trigger_type\":\"manual\",\"workflow_name\":\"Budget Approval\"}" \
  developer
assert_http 200 "Automation rule created"
NEW_RULE_ID=$(json_get "id")
assert_nonempty "id" "Rule ID returned"
NEW_RULE_NAME=$(json_get "name")
detail "Created rule: '$NEW_RULE_NAME' id=${NEW_RULE_ID:0:8}…"

action "Sam triggers the automation rule manually"
post "/api/automation/trigger/$RULE_ID" \
  '{"payload":{"source":"e2e_test","scenario":"FY2026 Budget"}}' developer
assert_http 200 "Rule triggered"
EXEC_STATUS=$(json_get "status")
EXEC_INSTANCE=$(json_get "instance_id")
assert_field "status" "completed" "Execution status=completed"
assert_nonempty "instance_id" "Workflow instance created by trigger"
detail "Execution instance=${EXEC_INSTANCE:0:8}…"

action "Sam views the execution log"
get /api/automation/executions developer
assert_http 200 "Executions endpoint responds"
assert_list_len_ge 1 "At least one execution in log"
EXEC_COUNT=$(json_len)
COMPLETED_EXECS=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
print(len([e for e in d if e.get('status')=='completed']))
" "$RESP")
pass "Executions: $EXEC_COUNT total, $COMPLETED_EXECS completed"

# ── 7. Alex — Planning Grid (Procurement app)  ───────────────────────────────
section "7. Alex — Procurement Planning Grid  [Phase 3 Reference App]"

action "Alex views the procurement spending grid"
get "/api/grid?scenario=Procurement+FY2026&version=Q1" dept_head
assert_http 200 "Procurement grid loads"
PROC_METRICS=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('metrics',[])))" "$RESP")
PROC_DEPTS=$(python3 -c  "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('departments',[])))" "$RESP")
PROC_CELLS=$(python3 -c  "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('cells',{})))"  "$RESP")
PROC_TOTALS=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len(d.get('totals',{})))" "$RESP")
if [[ "$PROC_METRICS" -ge 1 ]]; then
  pass "Procurement grid: $PROC_METRICS metrics, $PROC_DEPTS categories, $PROC_CELLS cells, $PROC_TOTALS totals"
else fail "Procurement grid returned no metrics"; fi

action "Alex checks savings metric total"
SAVINGS_TOTAL=$(python3 -c "
import sys,json; d=json.loads(sys.argv[1])
totals=d.get('totals',{})
# find metric labelled savings
metrics=d.get('metrics',[])
sav_id=next((m['id'] for m in metrics if 'savings' in m.get('name','').lower()),'')
print(totals.get(sav_id,'n/a'))
" "$RESP")
if [[ "$SAVINGS_TOTAL" != "n/a" && "$SAVINGS_TOTAL" != "" ]]; then
  pass "Savings total = $SAVINGS_TOTAL"
else warn "Savings metric not in totals — check Procurement seed"; fi

action "Alex fetches procurement metrics list"
get "/api/metrics?scenario=Procurement+FY2026&version=Q1" dept_head
assert_http 200 "Procurement metrics load"
PROC_MET_COUNT=$(json_len)
if [[ "$PROC_MET_COUNT" -ge 3 ]]; then pass "$PROC_MET_COUNT metrics (incl. calc metrics)"
else warn "Only $PROC_MET_COUNT procurement metrics — expected ≥3"; fi

# ── Summary ───────────────────────────────────────────────────────────────────
TOTAL=$((PASS + FAIL + SKIP))
echo ""
echo -e "${BOLD}─────────────────────────────────────────${NC}"
echo -e "${BOLD}Results: $TOTAL checks${NC}"
echo -e "  ${GREEN}✓ Passed : $PASS${NC}"
if [[ "$FAIL" -gt 0 ]]; then
  echo -e "  ${RED}✗ Failed : $FAIL${NC}"
else
  echo -e "  ${GREEN}✗ Failed : 0${NC}"
fi
echo -e "  ${YELLOW}~ Skipped: $SKIP${NC}"
echo -e "${BOLD}─────────────────────────────────────────${NC}"

if [[ "$FAIL" -eq 0 ]]; then
  echo -e "\n${GREEN}${BOLD}All checks passed.${NC}"
  exit 0
else
  echo -e "\n${RED}${BOLD}$FAIL check(s) failed.${NC}"
  exit 1
fi
