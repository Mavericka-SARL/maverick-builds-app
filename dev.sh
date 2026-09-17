#!/usr/bin/env bash
# dev.sh — start the gateway in local dev mode.
# Usage: bash dev.sh
set -euo pipefail

export DEV_MODE=true
export DATABASE_URL="${DATABASE_URL:-postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable}"

echo "Starting gateway (DEV_MODE=true) on :8080"
echo "DB: $DATABASE_URL"
exec go run ./cmd/gateway
