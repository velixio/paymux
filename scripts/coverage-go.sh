#!/usr/bin/env bash
# Cover every Go package, including execution through integration tests.
# Set PAYMUX_TEST_POSTGRES_URL to include PostgreSQL paths. Nothing is excluded.
set -uo pipefail
cd "$(dirname "$0")/.."
minimum="${PAYMUX_COVERAGE_MIN:-100}"
if ! awk -v value="$minimum" 'BEGIN { exit !(value ~ /^[0-9]+([.][0-9]+)?$/ && value >= 0 && value <= 100) }'; then
  echo "PAYMUX_COVERAGE_MIN must be a number between 0 and 100" >&2
  exit 2
fi
mkdir -p coverage/go
status=0
go test -timeout 30m -covermode=atomic -coverpkg=./... -coverprofile=coverage/go/coverage.out ./... || status=$?
if [ -s coverage/go/coverage.out ]; then
  go tool cover -func=coverage/go/coverage.out > coverage/go/functions.txt || status=1
  go tool cover -html=coverage/go/coverage.out -o coverage/go/index.html || status=1
  awk -v minimum="$minimum" '
    NR > 1 { statements[$1] = $2; if ($3 > 0) hit[$1] = 1 }
    END {
      for (block in statements) { total += statements[block]; if (hit[block]) covered += statements[block] }
      percent = total ? 100 * covered / total : 0
      printf "{\"coveredStatements\":%d,\"totalStatements\":%d,\"percent\":%.4f,\"target\":%g}\n", covered, total, percent, minimum > "coverage/go/summary.json"
      printf "Go coverage: %d/%d statements (%.4f%%); target %g%%\n", covered, total, percent, minimum
      if (percent < minimum) exit 1
    }' coverage/go/coverage.out || status=1
fi
exit "$status"
