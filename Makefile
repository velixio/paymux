VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: build test test-pg test-all fmt ci run-dev

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/paymux/paymux.Version=$(VERSION)" -o bin/paymux ./cmd/paymux

test:
	go test -race -timeout 30m ./...

test-pg:
	@set -e; \
	if [ -n "$$PAYMUX_TEST_POSTGRES_URL" ]; then \
		PAYMUX_REQUIRE_PG=1 go test -race -timeout 30m ./...; \
	elif command -v docker >/dev/null 2>&1; then \
		cid=$$(docker run -d --rm -e POSTGRES_PASSWORD=paymux -e POSTGRES_DB=paymux -p 127.0.0.1::5432 postgres:17); \
		trap 'docker rm -f $$cid >/dev/null 2>&1' EXIT; \
		port=$$(docker port $$cid 5432/tcp | head -n1 | sed 's/.*://'); \
		for i in $$(seq 1 60); do docker exec $$cid pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done; \
		PAYMUX_TEST_POSTGRES_URL="postgres://postgres:paymux@127.0.0.1:$$port/paymux?sslmode=disable" PAYMUX_REQUIRE_PG=1 go test -race -timeout 30m ./...; \
	else \
		echo "set PAYMUX_TEST_POSTGRES_URL or install docker" >&2; exit 1; \
	fi

test-all: test test-pg

fmt:
	gofmt -w .

ci:
	scripts/ci.sh

run-dev:
	go run ./cmd/paymux serve
