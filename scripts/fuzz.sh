#!/usr/bin/env bash
# Runs every fuzz target for real. Usage: bash scripts/fuzz.sh [fuzztime]   (default 2m each)
# CI does not run this: there the targets run as ordinary tests over their seed
# corpus (go test ./...), including every input a past fuzz run saved under
# testdata/fuzz/.
set -euo pipefail
cd "$(dirname "$0")/.."

FUZZTIME="${1:-2m}"

run() { # package target
  printf '\n==> %s %s (%s)\n' "$1" "$2" "$FUZZTIME"
  go test -run xxx -fuzz "^$2\$" -fuzztime "$FUZZTIME" "$1"
}

for c in razorpay payu zoho whop; do
  run "./connectors/$c" FuzzVerifyWebhook
  run "./connectors/$c" FuzzParseWebhook
done
run ./pkg/connector/money FuzzParseDecimal
run ./pkg/connector/money FuzzFormatDecimal
run ./internal/tax FuzzValidGSTIN
run ./internal/tax FuzzClassify
run ./internal/tax FuzzAmountInWordsINR
