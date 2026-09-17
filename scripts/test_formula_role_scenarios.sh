#!/usr/bin/env bash
# Runs formula-calculation journeys by user role.
#
# These tests are intentionally server-independent. They exercise the shared
# formula engine and in-memory planning/form-posting scenarios that map to:
#   - Sam / developer: author formulas and validate refs
#   - Alex / dept_head: view calculated metrics in dimensional grids
#   - Jordan / finance: approve form records that post to input metrics
#   - Pat / platform_admin: validate Form Records integration mapping
#
# Usage:
#   bash scripts/test_formula_role_scenarios.sh
#   bash scripts/test_formula_role_scenarios.sh developer
#   bash scripts/test_formula_role_scenarios.sh dept_head
#   bash scripts/test_formula_role_scenarios.sh finance
#   bash scripts/test_formula_role_scenarios.sh platform_admin

set -euo pipefail

ROLE="${1:-all}"

case "$ROLE" in
  all)
    PATTERN='TestRole'
    ;;
  developer|sam)
    PATTERN='TestRoleDeveloperFormulaAuthor'
    ;;
  dept_head|alex)
    PATTERN='TestRoleDeptHeadGridMetricCalculations'
    ;;
  finance|jordan)
    PATTERN='TestRoleFinanceFormApprovalPosting'
    ;;
  platform_admin|pat)
    PATTERN='TestRolePlatformAdminFormRecordsIntegrationMapping'
    ;;
  *)
    echo "Unknown role: $ROLE" >&2
    echo "Expected one of: all, developer, dept_head, finance, platform_admin" >&2
    exit 2
    ;;
esac

echo "Formula role scenario tests"
echo "Role: $ROLE"
echo "Pattern: $PATTERN"
echo

export GOCACHE="${GOCACHE:-/private/tmp/mavericks_engine_go_cache}"
mkdir -p "$GOCACHE"

go test ./internal/formula -run "$PATTERN" -v
