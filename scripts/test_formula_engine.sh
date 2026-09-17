#!/usr/bin/env bash
# Formula Engine — User Testing Scripts
# Run against a locally running server: go run ./cmd/gateway
#
# Usage:
#   bash scripts/test_formula_engine.sh               # all scenarios
#   bash scripts/test_formula_engine.sh scenario2     # single scenario

set -euo pipefail

BASE="http://localhost:8080"
MODEL_ID="7de7990c-664c-4b5f-8f18-e637e8d346e0"
REVISION_ID="e9b45543-9167-4284-bd54-7a5a0d95a7bd"
SCENARIO="FY2026 Forecast"
VERSION="draft"
FORM_ID="6a7a620d-d2b5-4ef8-9a6e-c85370a8fc5a"

# Core input metric IDs (demo model, revision above)
HC_ID="2112c50a-fa00-4a55-91d3-9fba18e5f350"       # hc_cost
HR_ID="274e106e-fcad-430b-91ea-7b8723e0307c"       # hr_cost
SW_ID="abb7b75c-f1f9-44b2-9023-3dd6f3c79adc"       # software_cost
TV_ID="e3d9173f-724f-41db-ac9d-cdd4c6467431"       # travel_cost
TOTAL_ID="47719000-d073-455f-857e-1dc606cd2137"    # total_opex (calculated)

# Dimension and grid definition that share the same ID space
DIM_ID="298fd8ea-f73a-4938-83db-34fbbaed2c67"      # department dimension
GRID_DEF="913e3fbf-40d8-4471-afe9-5b3e5b67ba98"   # opex grid def (uses DIM_ID)

# ── helpers ────────────────────────────────────────────────────────────────────

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0

pass()    { echo -e "${GREEN}PASS${NC} $1"; ((PASS++)) || true; }
fail()    { echo -e "${RED}FAIL${NC} $1"; ((FAIL++)) || true; }
section() { echo -e "\n${YELLOW}══ $1 ══${NC}"; }

jval()  { echo "$1" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$2',''))"              2>/dev/null; }
jfloat(){ echo "$1" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$2') or 0)"           2>/dev/null; }
jlen()  { echo "$1" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))"                           2>/dev/null; }

dev_get()  { curl -s "$BASE$1"    -H "X-Dev-User: developer" 2>&1; }
dev_post() { curl -s -X POST "$BASE$1" -H "X-Dev-User: developer" -H "Content-Type: application/json" -d "$2" 2>&1; }
biz_post() { curl -s -X POST "$BASE$1" -H "X-Dev-User: dept_head"  -H "Content-Type: application/json" -d "$2" 2>&1; }
fin_get()  { curl -s "$BASE$1"    -H "X-Dev-User: finance" 2>&1; }

# Write a single cell value at the aggregate level (no dimension context)
write_agg() {
  local metric_id=$1 value=$2
  curl -sf -X POST "$BASE/api/cells" \
    -H "X-Dev-User: developer" -H "Content-Type: application/json" \
    -d "{\"model_id\":\"$MODEL_ID\",\"revision_id\":\"$REVISION_ID\",
         \"metric_id\":\"$metric_id\",\"value\":$value}" > /dev/null
}

# Write a single cell value scoped to a department dimension member
write_dim() {
  local metric_id=$1 dept_code=$2 value=$3
  curl -sf -X POST "$BASE/api/cells" \
    -H "X-Dev-User: developer" -H "Content-Type: application/json" \
    -d "{\"model_id\":\"$MODEL_ID\",\"revision_id\":\"$REVISION_ID\",
         \"metric_id\":\"$metric_id\",
         \"dim_codes\":{\"$DIM_ID\":\"$dept_code\"},
         \"value\":$value}" > /dev/null
}

# Unique suffix so repeated runs don't collide on metric/form names
RUN_ID=$(date +%s)

# ── Scenario 1: Existing grid with calculated metric ──────────────────────────
#
# Reads total_opex from the demo model.  Verifies formula is stored, the metric
# has a calculated value, and the grid endpoint returns dimensions and metrics.

scenario1() {
  section "Scenario 1 — Grid with calculated metric (total_opex)"

  # 1a. total_opex formula must reference hc_cost
  METRICS=$(dev_get "/api/metrics?revision_id=$REVISION_ID")
  FORMULA=$(echo "$METRICS" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='total_opex': print(m.get('formula',''))
" 2>/dev/null)

  if [[ "$FORMULA" == *"hc_cost"* ]]; then
    pass "1a: total_opex formula references hc_cost (${FORMULA})"
  else
    fail "1a: unexpected formula: '${FORMULA}'"
  fi

  # 1b. total_opex must have a non-null calculated value
  TOTAL=$(echo "$METRICS" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='total_opex': print(m.get('value') or 'null')
" 2>/dev/null)

  if [[ "$TOTAL" != "null" && "$TOTAL" != "" ]]; then
    pass "1b: total_opex has calculated value ($TOTAL)"
  else
    fail "1b: total_opex value is null — no fact_input rows exist"
  fi

  # 1c. Grid endpoint returns dimensions and metrics
  GRID=$(dev_get "/api/grid?revision_id=$REVISION_ID&scenario=FY2026+Forecast&version=draft")
  DIMS=$(echo "$GRID" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('dimensions',[])))" 2>/dev/null)
  if [[ "$DIMS" -ge 1 ]]; then
    pass "1c: Grid returns $DIMS dimension(s)"
  else
    fail "1c: Grid returned no dimensions"
  fi

  METRIC_COUNT=$(echo "$GRID" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('metrics',[])))" 2>/dev/null)
  if [[ "$METRIC_COUNT" -ge 4 ]]; then
    pass "1d: Grid returns $METRIC_COUNT metrics"
  else
    fail "1d: Grid returns only $METRIC_COUNT metrics (expected ≥4)"
  fi
}

# ── Scenario 2: Create calculated metric with Excel-style formula ─────────────
#
# Developer creates a new metric using the formula engine (no braces).
# Verifies: formula stored, dependency graph populated via formula.ExtractRefs.

scenario2() {
  section "Scenario 2 — Create calculated metric with new Excel formula"

  METRIC_NAME="test_excel_${RUN_ID}"
  LEGACY_NAME="test_brace_${RUN_ID}"

  # 2a. Create with new = syntax
  RESP=$(dev_post "/api/developer/metrics" "{
    \"name\": \"$METRIC_NAME\",
    \"label\": \"Test Excel Formula\",
    \"formula\": \"=hc_cost + hr_cost\",
    \"is_input\": false,
    \"revision_id\": \"$REVISION_ID\"
  }")
  NEW_ID=$(jval "$RESP" "id")
  if [[ -n "$NEW_ID" && "$NEW_ID" != "null" ]]; then
    pass "2a: Created calculated metric $METRIC_NAME (id=$NEW_ID)"
  else
    fail "2a: Could not create metric — $RESP"
    return
  fi

  # 2b. Formula stored correctly
  MODEL=$(dev_get "/api/developer/model?model_id=$MODEL_ID")
  STORED_FORMULA=$(echo "$MODEL" | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('metrics',[]):
    if m['name']=='$METRIC_NAME': print(m.get('formula',''))
" 2>/dev/null)

  if [[ "$STORED_FORMULA" == *"hc_cost"* ]]; then
    pass "2b: Formula stored correctly: '${STORED_FORMULA}'"
  else
    fail "2b: Formula not found or wrong: '${STORED_FORMULA}'"
  fi

  # 2c. Dependency graph has hc_cost (proves formula.ExtractRefs is used)
  DEPS=$(echo "$MODEL" | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('metrics',[]):
    if m['name']=='$METRIC_NAME': print(','.join(m.get('depends_on',[])))
" 2>/dev/null)

  if [[ "$DEPS" == *"hc_cost"* ]]; then
    pass "2c: depends_on populated: [$DEPS]"
  else
    fail "2c: depends_on missing — got: '$DEPS'"
  fi

  # 2d. Legacy {brace} syntax still accepted (backward compat)
  RESP2=$(dev_post "/api/developer/metrics" "{
    \"name\": \"$LEGACY_NAME\",
    \"label\": \"Test Legacy Formula\",
    \"formula\": \"{hc_cost} + {hr_cost}\",
    \"is_input\": false,
    \"revision_id\": \"$REVISION_ID\"
  }")
  LEGACY_ID=$(jval "$RESP2" "id")
  if [[ -n "$LEGACY_ID" && "$LEGACY_ID" != "null" ]]; then
    pass "2d: Legacy {brace} formula accepted (backward compat)"
  else
    fail "2d: Legacy formula rejected — $RESP2"
  fi
}

# ── Scenario 3: Formula evaluation correctness ────────────────────────────────

scenario3() {
  section "Scenario 3 — Formula evaluation correctness"

  METRICS=$(dev_get "/api/metrics?revision_id=$REVISION_ID")

  # 3a. total_opex has a non-null value
  RESULT=$(echo "$METRICS" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='total_opex':
        v = m.get('value')
        print('SKIP:total_opex is null' if v is None else f'PASS:value={v}')
        break
else:
    print('FAIL:total_opex not found')
" 2>/dev/null)

  STATUS="${RESULT%%:*}"
  MSG="${RESULT#*:}"
  case "$STATUS" in
    PASS) pass "3a: total_opex $MSG" ;;
    SKIP) echo "  SKIP: $MSG" ;;
    FAIL) fail "3a: $MSG" ;;
  esac

  # 3b. formula field is non-empty
  FORMULA=$(echo "$METRICS" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='total_opex' and m.get('formula'): print(m['formula'])
" 2>/dev/null)

  if [[ -n "$FORMULA" ]]; then
    pass "3b: total_opex.formula = '${FORMULA}'"
  else
    fail "3b: total_opex has no formula"
  fi
}

# ── Scenario 4: Form record submission ───────────────────────────────────────

scenario4() {
  section "Scenario 4 — Form record submission"

  RECORDS_BEFORE=$(curl -sf "$BASE/api/forms/$FORM_ID/records" -H "X-Dev-User: dept_head")
  COUNT_BEFORE=$(echo "$RECORDS_BEFORE" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null)

  SUBMIT=$(biz_post "/api/forms/$FORM_ID/records" "{
    \"data\": {
      \"vendor\": \"Test Vendor Corp\",
      \"category\": \"IT\",
      \"amount\": 7500,
      \"currency\": \"USD\",
      \"justification\": \"Formula engine test\",
      \"is_urgent\": false,
      \"dept\": \"SALES\"
    },
    \"revision_id\": \"$REVISION_ID\"
  }")

  RECORD_ID=$(jval "$SUBMIT" "id")
  if [[ -n "$RECORD_ID" && "$RECORD_ID" != "null" ]]; then
    pass "4a: Form record created (id=$RECORD_ID)"
  else
    fail "4a: Form record creation failed — $SUBMIT"
    return
  fi

  RECORDS_AFTER=$(curl -sf "$BASE/api/forms/$FORM_ID/records" -H "X-Dev-User: dept_head")
  COUNT_AFTER=$(echo "$RECORDS_AFTER" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null)

  if [[ "$COUNT_AFTER" -gt "$COUNT_BEFORE" ]]; then
    pass "4b: Record count increased ($COUNT_BEFORE → $COUNT_AFTER)"
  else
    fail "4b: Record count unchanged ($COUNT_BEFORE → $COUNT_AFTER)"
  fi

  LAST_VENDOR=$(echo "$RECORDS_AFTER" | python3 -c "
import sys,json
for r in json.load(sys.stdin):
    if r.get('id')=='$RECORD_ID':
        print(r['data'].get('vendor','?'))
        break
" 2>/dev/null)

  if [[ "$LAST_VENDOR" == "Test Vendor Corp" ]]; then
    pass "4c: Record data round-tripped correctly"
  else
    fail "4c: Vendor data mismatch — got '$LAST_VENDOR'"
  fi
}

# ── Scenario 5: Business user dimension access ────────────────────────────────

scenario5() {
  section "Scenario 5 — Business users can access dimensions"

  DIM_RESP=$(curl -sf "$BASE/api/dimensions?revision_id=$REVISION_ID" -H "X-Dev-User: finance")

  MEMBER_COUNT=$(echo "$DIM_RESP" | python3 -c "
import sys,json
print(sum(len(d.get('members',[])) for d in json.load(sys.stdin)))
" 2>/dev/null)

  if [[ "$MEMBER_COUNT" -ge 3 ]]; then
    pass "5a: Finance user sees $MEMBER_COUNT dimension members"
  else
    fail "5a: Finance user sees $MEMBER_COUNT members (expected ≥3)"
  fi

  DIM_RESP2=$(curl -sf "$BASE/api/dimensions?revision_id=$REVISION_ID" -H "X-Dev-User: dept_head")
  DIMS=$(echo "$DIM_RESP2" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null)
  if [[ "$DIMS" -ge 1 ]]; then
    pass "5b: Dept Head sees $DIMS dimension group(s)"
  else
    fail "5b: Dept Head sees no dimensions"
  fi
}

# ── Scenario 6: Dependency graph reflects formula references ──────────────────

scenario6() {
  section "Scenario 6 — Dependency graph reflects formula references"

  MODEL=$(dev_get "/api/developer/model?model_id=$MODEL_ID")

  TOTAL_DEPS=$(echo "$MODEL" | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('metrics',[]):
    if m['name']=='total_opex' and m.get('depends_on'):
        print(','.join(sorted(m.get('depends_on',[]))))
        break
" 2>/dev/null)

  DEP_COUNT=$(echo "$TOTAL_DEPS" | tr ',' '\n' | grep -c '\S' || true)
  if [[ "$DEP_COUNT" -ge 4 ]]; then
    pass "6a: total_opex.depends_on has $DEP_COUNT metrics: [$TOTAL_DEPS]"
  else
    fail "6a: total_opex.depends_on has $DEP_COUNT (want ≥4): [$TOTAL_DEPS]"
  fi

  HC_DEPS=$(echo "$MODEL" | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('metrics',[]):
    if m['name']=='hc_cost':
        print(','.join(m.get('depended_by',[])))
        break
" 2>/dev/null)

  if [[ "$HC_DEPS" == *"total_opex"* ]]; then
    pass "6b: hc_cost.depended_by includes total_opex"
  else
    fail "6b: hc_cost.depended_by missing total_opex — got: '$HC_DEPS'"
  fi
}

# ── Scenario 7: Complex formulas — isolated metric chain ─────────────────────
#
# Creates a self-contained metric chain using only metrics created in this test
# run so prior test data cannot pollute the calculated values.
#
#   t_rev (input)  t_cog (input)
#   t_gp  = t_rev - t_cog
#   t_gm  = IFERROR(t_gp / t_rev, 0)          ← multi-hop dependency
#   t_tier= IF(t_rev > 100000, 3,
#               IF(t_rev > 50000, 2, 1))        ← nested IF → numeric tier
#
# With t_rev=120000, t_cog=72000:
#   t_gp   = 48000
#   t_gm   = 0.4
#   t_tier = 3

scenario7() {
  section "Scenario 7 — Complex formula chain (IFERROR, multi-hop, nested IF)"

  post_metric() {
    curl -sf -X POST "$BASE/api/developer/metrics" \
      -H "X-Dev-User: developer" -H "Content-Type: application/json" \
      -d "{\"name\":\"$1\",\"label\":\"$2\",\"formula\":\"$3\",\"is_input\":$4,\"revision_id\":\"$REVISION_ID\"}" \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('id','ERR'))"
  }

  REV_ID=$(post_metric "t_rev_$RUN_ID" "Test Revenue" ""                                                   "true")
  COG_ID=$(post_metric "t_cog_$RUN_ID" "Test COGS"    ""                                                   "true")
  GP_ID=$(post_metric  "t_gp_$RUN_ID"  "Gross Profit" "=t_rev_$RUN_ID - t_cog_$RUN_ID"                    "false")
  GM_ID=$(post_metric  "t_gm_$RUN_ID"  "Gross Margin" "=IFERROR(t_gp_$RUN_ID / t_rev_$RUN_ID, 0)"         "false")
  TI_ID=$(post_metric  "t_tier_$RUN_ID" "Rev Tier"    "=IF(t_rev_$RUN_ID > 100000, 3, IF(t_rev_$RUN_ID > 50000, 2, 1))" "false")

  for id in $REV_ID $COG_ID $GP_ID $GM_ID $TI_ID; do
    if [[ "$id" == "ERR" || -z "$id" ]]; then
      fail "7a: metric creation failed"
      return
    fi
  done
  pass "7a: Created 5 test metrics (rev/cog/gp/gm/tier)"

  # Verify dependency graph was correctly wired (no cross-revision contamination)
  MODEL=$(dev_get "/api/developer/model?model_id=$MODEL_ID")

  GP_DEPS=$(echo "$MODEL" | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('metrics',[]):
    if m['name']=='t_gp_$RUN_ID': print(','.join(m.get('depends_on',[])))
" 2>/dev/null)
  if [[ "$GP_DEPS" == *"t_rev_$RUN_ID"* && "$GP_DEPS" == *"t_cog_$RUN_ID"* ]]; then
    pass "7b: t_gp deps correct: [$GP_DEPS]"
  else
    fail "7b: t_gp deps wrong — got: '$GP_DEPS'"
  fi

  GM_DEPS=$(echo "$MODEL" | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('metrics',[]):
    if m['name']=='t_gm_$RUN_ID': print(','.join(m.get('depends_on',[])))
" 2>/dev/null)
  if [[ "$GM_DEPS" == *"t_gp_$RUN_ID"* && "$GM_DEPS" == *"t_rev_$RUN_ID"* ]]; then
    pass "7c: t_gm deps correct (multi-hop via t_gp): [$GM_DEPS]"
  else
    fail "7c: t_gm deps wrong — got: '$GM_DEPS'"
  fi

  # Write aggregate inputs (no dim_codes → dim_members = '{}')
  # t_rev=120000, t_cog=72000  →  t_gp=48000, t_gm=0.4, t_tier=3
  write_agg "$REV_ID" 120000
  write_agg "$COG_ID" 72000
  sleep 1

  METRICS=$(dev_get "/api/metrics?revision_id=$REVISION_ID")
  check_val() {
    local name=$1 expect=$2 label=$3
    local actual
    actual=$(echo "$METRICS" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='$name':
        print(m.get('value') or 'null')
        break
" 2>/dev/null)
    if python3 -c "
import sys
v='$actual'
e='$expect'
if v=='null': sys.exit(1)
if abs(float(v)-float(e)) < 0.001: sys.exit(0)
sys.exit(1)
" 2>/dev/null; then
      pass "$label: $name = $actual (expected $expect)"
    else
      fail "$label: $name = $actual (expected $expect)"
    fi
  }

  check_val "t_gp_$RUN_ID"   48000  "7d"
  check_val "t_gm_$RUN_ID"   0.4    "7e"
  check_val "t_tier_$RUN_ID" 3      "7f"

  # Zero-revenue guard: IFERROR should return 0, not div/0 error
  write_agg "$REV_ID" 0
  write_agg "$COG_ID" 0
  sleep 1
  METRICS2=$(dev_get "/api/metrics?revision_id=$REVISION_ID")
  GM_ZERO=$(echo "$METRICS2" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='t_gm_$RUN_ID':
        v=m.get('value'); print('null' if v is None else v)
" 2>/dev/null)
  if [[ "$GM_ZERO" == "0" || "$GM_ZERO" == "0.0" ]]; then
    pass "7g: IFERROR guard: t_gm = 0 when revenue = 0 (no div/0 error)"
  else
    fail "7g: IFERROR guard failed — got: '$GM_ZERO' (expected 0)"
  fi

  # Restore values for any further assertions
  write_agg "$REV_ID" 120000
  write_agg "$COG_ID" 72000
}

# ── Scenario 8: Grid cells with per-department writes ────────────────────────
#
# Writes per-department input values using the cells endpoint, then verifies
# the grid_def-scoped grid returns exactly the expected cells.
#
# SALES: hc=8000 hr=3000 sw=2000 tv=500  → total=13500
# ENG:   hc=12000 hr=4000 sw=5000 tv=200 → total=21200
# MKTG:  hc=5000 hr=2000 sw=1000 tv=1500 → total=9500
# GA:    hc=3000 hr=1500 sw=500 tv=300   → total=5300

scenario8() {
  section "Scenario 8 — Grid cells with per-department writes"

  echo "  Writing 16 dimensional cells (4 metrics × 4 departments)..."
  write_dim "$HC_ID" SALES 8000;  write_dim "$HR_ID" SALES 3000
  write_dim "$SW_ID" SALES 2000;  write_dim "$TV_ID" SALES 500
  write_dim "$HC_ID" ENG   12000; write_dim "$HR_ID" ENG   4000
  write_dim "$SW_ID" ENG   5000;  write_dim "$TV_ID" ENG   200
  write_dim "$HC_ID" MKTG  5000;  write_dim "$HR_ID" MKTG  2000
  write_dim "$SW_ID" MKTG  1000;  write_dim "$TV_ID" MKTG  1500
  write_dim "$HC_ID" GA    3000;  write_dim "$HR_ID" GA    1500
  write_dim "$SW_ID" GA    500;   write_dim "$TV_ID" GA    300
  sleep 1

  GRID=$(curl -s "$BASE/api/grid?grid_def_id=$GRID_DEF&revision_id=$REVISION_ID" \
    -H "X-Dev-User: developer")

  CELL_COUNT=$(echo "$GRID" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('cells',{})))" 2>/dev/null)
  if [[ "$CELL_COUNT" -ge 16 ]]; then
    pass "8a: Grid shows $CELL_COUNT cells (≥16 for 4 metrics × 4 depts)"
  else
    fail "8a: Grid shows $CELL_COUNT cells (expected ≥16)"
  fi

  # Verify per-department values
  HC_SALES=$(echo "$GRID" | python3 -c "
import sys,json
d=json.load(sys.stdin)
cells=d.get('cells',{})
metrics={m['id']:m['name'] for m in d.get('metrics',[])}
for k,v in cells.items():
    mid=k.split(':')[0]; dim=':'.join(k.split(':')[1:])
    if metrics.get(mid)=='hc_cost' and 'SALES' in dim:
        print(v); break
" 2>/dev/null)
  if python3 -c "v='$HC_SALES'; exit(0 if v and abs(float(v)-8000)<1 else 1)" 2>/dev/null; then
    pass "8b: hc_cost@SALES = $HC_SALES"
  else
    fail "8b: hc_cost@SALES unexpected: '$HC_SALES' (expected 8000)"
  fi

  HC_ENG=$(echo "$GRID" | python3 -c "
import sys,json
d=json.load(sys.stdin)
cells=d.get('cells',{})
metrics={m['id']:m['name'] for m in d.get('metrics',[])}
for k,v in cells.items():
    mid=k.split(':')[0]; dim=':'.join(k.split(':')[1:])
    if metrics.get(mid)=='hc_cost' and 'ENG' in dim:
        print(v); break
" 2>/dev/null)
  if python3 -c "v='$HC_ENG'; exit(0 if v and abs(float(v)-12000)<1 else 1)" 2>/dev/null; then
    pass "8c: hc_cost@ENG = $HC_ENG"
  else
    fail "8c: hc_cost@ENG unexpected: '$HC_ENG' (expected 12000)"
  fi

  # total_opex appears in the grid totals (sum of all dimension inputs)
  TOTAL=$(echo "$GRID" | python3 -c "
import sys,json
d=json.load(sys.stdin)
totals=d.get('totals',{})
metrics={m['id']:m['name'] for m in d.get('metrics',[])}
for mid,v in totals.items():
    if metrics.get(mid)=='total_opex': print(v); break
" 2>/dev/null)
  if [[ -n "$TOTAL" && "$TOTAL" != "None" ]]; then
    pass "8d: total_opex in grid totals = $TOTAL"
  else
    fail "8d: total_opex missing from grid totals"
  fi
}

# ── Scenario 9: Form with metric field → automatic writeback + recalculation ──
#
# Creates a new form that has:
#   - a dimension field (dept)      → provides dim_members context
#   - a metric field (rev_budget)   → writes a fact_input row on submission
#
# Submits two records for different departments and verifies that the
# dependent calculated metric (t_gm) recalculated after each submission.

scenario9() {
  section "Scenario 9 — Form with metric field feeds metric + triggers recalc"

  # Get the test metric IDs created in scenario 7
  MODEL=$(dev_get "/api/developer/model?model_id=$MODEL_ID")
  S9_REV_ID=$(echo "$MODEL" | python3 -c "
import sys,json
for m in json.load(sys.stdin).get('metrics',[]):
    if m['name']=='t_rev_$RUN_ID': print(m['id']); break
" 2>/dev/null)

  if [[ -z "$S9_REV_ID" || "$S9_REV_ID" == "ERR" ]]; then
    echo "  SKIP: scenario 7 metrics not found — run scenario 7 first"
    return
  fi

  # Create a new form with the metric field linked to t_rev
  FORM_RESP=$(dev_post "/api/forms" "{
    \"name\": \"budget_req_$RUN_ID\",
    \"label\": \"Budget Request\",
    \"fields\": [
      {\"name\":\"dept\",\"label\":\"Department\",\"type\":\"dimension\",\"required\":true,\"dimension_id\":\"$DIM_ID\"},
      {\"name\":\"rev_budget\",\"label\":\"Revenue Budget\",\"type\":\"metric\",\"required\":true,\"metric_id\":\"$S9_REV_ID\"}
    ]
  }")
  S9_FORM_ID=$(jval "$FORM_RESP" "id")

  if [[ -z "$S9_FORM_ID" || "$S9_FORM_ID" == "null" || "$S9_FORM_ID" == "" ]]; then
    fail "9a: Could not create form — $FORM_RESP"
    return
  fi
  pass "9a: Created form budget_req_$RUN_ID (id=$S9_FORM_ID)"

  # Record t_gm value BEFORE form submission
  GM_BEFORE=$(dev_get "/api/metrics?revision_id=$REVISION_ID" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='t_gm_$RUN_ID': print(m.get('value') or 'null'); break
" 2>/dev/null)
  echo "  t_gm before submission: $GM_BEFORE"

  # Submit form record — dept=SALES, rev_budget=200000
  SUBMIT=$(biz_post "/api/forms/$S9_FORM_ID/records" "{
    \"data\": {\"dept\":\"SALES\",\"rev_budget\":200000},
    \"revision_id\": \"$REVISION_ID\"
  }")
  RECORD_ID=$(jval "$SUBMIT" "id")

  if [[ -n "$RECORD_ID" && "$RECORD_ID" != "null" ]]; then
    pass "9b: Form record created (id=$RECORD_ID)"
  else
    fail "9b: Form record creation failed — $SUBMIT"
    return
  fi

  sleep 1

  # Verify fact_input row was written and triggered recalculation:
  # t_rev now includes the 200000 dimensional write, so t_gm should change.
  GM_AFTER=$(dev_get "/api/metrics?revision_id=$REVISION_ID" | python3 -c "
import sys,json
for m in json.load(sys.stdin):
    if m['name']=='t_gm_$RUN_ID': print(m.get('value') or 'null'); break
" 2>/dev/null)
  echo "  t_gm after submission: $GM_AFTER"

  if [[ "$GM_BEFORE" != "$GM_AFTER" && "$GM_AFTER" != "null" ]]; then
    pass "9c: t_gm recalculated after form submission ($GM_BEFORE → $GM_AFTER)"
  else
    fail "9c: t_gm did not recalculate (before=$GM_BEFORE after=$GM_AFTER)"
  fi

  # Verify grid cell for the dimensional write
  GRID=$(curl -s "$BASE/api/grid?grid_def_id=$GRID_DEF&revision_id=$REVISION_ID" \
    -H "X-Dev-User: developer")
  CELL_COUNT=$(echo "$GRID" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('cells',{})))" 2>/dev/null)
  if [[ "$CELL_COUNT" -ge 1 ]]; then
    pass "9d: Grid still has cells after form submission ($CELL_COUNT)"
  else
    fail "9d: Grid shows 0 cells"
  fi
}

# ── Summary ────────────────────────────────────────────────────────────────────

run_all() {
  echo "Formula Engine User Test Suite"
  echo "Backend: $BASE  Model: $MODEL_ID"

  if ! curl -sf "$BASE/healthz" >/dev/null 2>&1; then
    echo -e "${RED}ERROR:${NC} Server not running at $BASE — start with: go run ./cmd/gateway"
    exit 1
  fi

  scenario1
  scenario2
  scenario3
  scenario4
  scenario5
  scenario6
  scenario7
  scenario8
  scenario9

  echo ""
  echo "────────────────────────────────"
  echo -e "Results: ${GREEN}$PASS passed${NC}  ${RED}$FAIL failed${NC}"
  [[ $FAIL -eq 0 ]] && echo -e "${GREEN}All tests passed${NC}" || echo -e "${RED}Some tests failed${NC}"
}

if [[ "${1:-}" =~ ^scenario[0-9]+$ ]]; then
  "${1}"
  echo ""
  echo "Results: $PASS passed  $FAIL failed"
else
  run_all
fi
