#!/usr/bin/env bash
#
# Runs the test suite and enforces the coverage gate (default 80%).
#
# The architecture document excludes generated code (sqlc, mockery) and cmd/
# from the count; tools/ and migrations/ are excluded as well, since they are
# development tooling and SQL assets rather than product code.
#
# -coverpkg=./... instruments every package, so a package without tests counts as
# 0% instead of disappearing from the report. That makes each test binary emit a
# profile for the whole module, so the profiles are merged by taking the highest
# count per block before the total is computed.
#
# Usage: scripts/coverage.sh [extra go test flags...]
# Environment: MIN_COVERAGE (default 80), COVERAGE_PROFILE (default coverage.out)

set -euo pipefail

extra_flags=("$@")
min_coverage="${MIN_COVERAGE:-80}"
profile="${COVERAGE_PROFILE:-coverage.out}"
raw="${profile%.out}.raw"

excluded='(^|/)(cmd|tools|migrations|test/integration|sqlcgen|mocks)/'

printf '==> go test ./... %s -coverpkg=./... -coverprofile=%s\n' "${extra_flags[*]:-}" "$raw"
if [ "${#extra_flags[@]}" -gt 0 ]; then
    go test ./... -coverpkg=./... -coverprofile="$raw" -covermode=atomic "${extra_flags[@]}"
else
    go test ./... -coverpkg=./... -coverprofile="$raw" -covermode=atomic
fi

echo "==> merging profiles and filtering generated code"
awk -v pattern="$excluded" '
    NR == 1 { header = $0; next }
    $1 ~ pattern { next }
    {
        count = $NF
        if (!($1 in seen) || count > seen[$1]) {
            seen[$1] = count
            line[$1] = $0
        }
    }
    END {
        print header
        for (key in line) print line[key]
    }
' "$raw" > "$profile"

total="$(go tool cover -func="$profile" | awk '/^total:/ { print $3 }' | tr -d '%')"
echo "==> total coverage: ${total}% (minimum ${min_coverage}%)"

awk -v total="$total" -v min="$min_coverage" 'BEGIN {
    if (total + 0 < min + 0) {
        printf "coverage %.1f%% is below the %.1f%% minimum\n", total, min > "/dev/stderr"
        exit 1
    }
}'

echo "==> coverage gate passed"
