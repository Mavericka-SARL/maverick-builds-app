#!/usr/bin/env bash
# CI's Go test step: go test ./... with internal/gateway split in two.
#
# internal/gateway is the suite's long pole: its 512 tests run one after
# another and took about 620 s under -race on the self-hosted runners
# (2026-10-26), against the 20-minute package limit, while every other
# package finished within 3 minutes. Its tests therefore run as two halves,
# each a process with its own Postgres container (internal/testdb), beside
# the other packages; together they cover every test once.
#
#   scripts/go-test-split.sh -race -timeout 20m
#
# Arguments go to every go test call. P (default 3) caps how many of the
# other packages run at once; the two halves run alongside them.
set -euo pipefail
cd "$(dirname "$0")/.."

pkg=./internal/gateway
# Every top-level test of the package, by name (TestMain is the harness).
tests=$(grep -hoE '^func Test[A-Za-z0-9_]*\(t \*testing\.T\)' internal/gateway/*_test.go \
  | sed -E 's/^func ([A-Za-z0-9_]+).*/\1/' | sort -u)
half() { echo "$tests" | awk -v k="$1" 'NR % 2 == k' | paste -sd'|' -; }
first=$(half 1)
second=$(half 0)
others=$(go list ./... | grep -v '/internal/gateway$')

go test -p "${P:-3}" "$@" $others &
rest=$!
go test "$@" -run "^(${first})\$" "$pkg" &
one=$!
go test "$@" -run "^(${second})\$" "$pkg" &
two=$!

failed=0
for p in "$rest" "$one" "$two"; do
  wait "$p" || failed=1
done
exit "$failed"
