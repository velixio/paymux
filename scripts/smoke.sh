#!/usr/bin/env bash
# Smoke test of the real binary: builds dist/paymux, starts it on a fresh data
# directory, completes first-run setup over HTTP, adds a simulator provider
# account, and drives a payment to success, a signed webhook to a local
# receiver, an invoice PDF and a refund (scripts/smoke.py does the driving).
#
#   bash scripts/smoke.sh
#
# It runs against SQLite, and against PostgreSQL as well when
# PAYMUX_TEST_POSTGRES_URL is set (it needs psql then, to make a scratch
# schema). It exits non-zero, with the server log, on the first failing step.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v python3 >/dev/null || { echo "smoke: python3 is required" >&2; exit 1; }

echo "==> building dist/paymux"
make build >/dev/null

free_port() {
	python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
PY
}

pid=""
tmp=""
schema=""
cleanup() {
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	if [ -n "$schema" ]; then
		psql "$PAYMUX_TEST_POSTGRES_URL" -q -c "DROP SCHEMA IF EXISTS \"$schema\" CASCADE" >/dev/null 2>&1 || true
	fi
	[ -n "$tmp" ] && rm -rf "$tmp"
	return 0
}
trap cleanup EXIT

run_one() {
	local db="$1"
	echo
	echo "################ smoke test: $db ################"
	tmp="$(mktemp -d)"
	local port iport key url=""
	port="$(free_port)"
	iport="$(free_port)"
	key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
	if [ "$db" = postgres ]; then
		schema="smoke_$(date +%s)_$$"
		psql "$PAYMUX_TEST_POSTGRES_URL" -q -c "CREATE SCHEMA \"$schema\"" >/dev/null
		case "$PAYMUX_TEST_POSTGRES_URL" in
		*\?*) url="${PAYMUX_TEST_POSTGRES_URL}&search_path=$schema" ;;
		*) url="${PAYMUX_TEST_POSTGRES_URL}?search_path=$schema" ;;
		esac
	fi

	env -i PATH="$PATH" HOME="$tmp" \
		PAYMUX_ENV=development \
		PAYMUX_BASE_URL="http://127.0.0.1:$port" \
		PAYMUX_LISTEN="127.0.0.1:$port" \
		PAYMUX_INTERNAL_LISTEN="127.0.0.1:$iport" \
		PAYMUX_DATA_DIR="$tmp/data" \
		PAYMUX_MASTER_KEY="$key" \
		PAYMUX_DEV_SKIP_2FA=true \
		PAYMUX_ALLOW_PRIVATE_WEBHOOKS=true \
		${url:+PAYMUX_DATABASE_URL="$url"} \
		dist/paymux serve >"$tmp/server.log" 2>&1 &
	pid=$!

	local i
	for i in $(seq 1 100); do
		if curl -fsS "http://127.0.0.1:$port/readyz" >/dev/null 2>&1; then break; fi
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "smoke: the server exited during start-up:" >&2
			cat "$tmp/server.log" >&2
			exit 1
		fi
		sleep 0.2
	done
	curl -fsS "http://127.0.0.1:$port/readyz" >/dev/null || { echo "smoke: the server never became ready" >&2; cat "$tmp/server.log" >&2; exit 1; }

	local code
	code="$(grep -o '"setup_code":"[a-z-]*"' "$tmp/server.log" | head -1 | cut -d'"' -f4)"
	if [ -z "$code" ]; then
		echo "smoke: no setup code in the server log:" >&2
		cat "$tmp/server.log" >&2
		exit 1
	fi

	if ! python3 scripts/smoke.py --base "http://127.0.0.1:$port" --code "$code"; then
		echo >&2
		echo "smoke: FAILED on $db; the last 60 lines of the server log:" >&2
		tail -60 "$tmp/server.log" >&2
		exit 1
	fi

	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	pid=""
	if [ -n "$schema" ]; then
		psql "$PAYMUX_TEST_POSTGRES_URL" -q -c "DROP SCHEMA IF EXISTS \"$schema\" CASCADE" >/dev/null 2>&1 || true
		schema=""
	fi
	rm -rf "$tmp"
	tmp=""
}

run_one sqlite
if [ -n "${PAYMUX_TEST_POSTGRES_URL:-}" ] && [ "${PAYMUX_SMOKE_SQLITE_ONLY:-}" != 1 ]; then
	if command -v psql >/dev/null; then
		run_one postgres
	else
		echo "smoke: PAYMUX_TEST_POSTGRES_URL is set but psql is not installed; skipping the PostgreSQL run" >&2
	fi
fi
echo
echo "smoke: all runs passed"
