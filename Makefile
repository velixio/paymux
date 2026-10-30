BUN ?= bun
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: build build-paymux build-home build-examples clean dev run-home test test-pg test-all fmt ci run-dev kill docker

GOBUILD = CGO_ENABLED=0 go build -trimpath

# Every binary into bin/: the hub, the public website and the examples.
build: build-paymux build-home build-examples
	@ls -1 bin/

# Remove build output only (what .gitignore marks as generated). Never touches
# data/, .env or node_modules.
clean:
	rm -rf bin web/static/dashboard coverage frontend/test-results frontend/playwright-report
	find cmd/paymux-home/dist -mindepth 1 ! -name .keep -exec rm -rf {} +

# The payment hub (with the dashboard frontend embedded).
build-paymux: frontend
	$(GOBUILD) -ldflags "-s -w -X github.com/velixio/paymux/pkg/hub.Version=$(VERSION)" -o bin/paymux ./cmd/paymux

# Public website: independent binary, no payment-hub configuration or database.
build-home: frontend-home
	$(GOBUILD) -ldflags "-s -w" -o bin/paymux-home ./cmd/paymux-home

# Example programs: an API client, a framework-mode hub and a remote connector.
build-examples:
	$(GOBUILD) -o bin/example-api-client ./examples/api-client
	$(GOBUILD) -o bin/example-framework ./examples/framework
	$(GOBUILD) -o bin/example-remote-connector ./examples/remote-connector

# Build everything, then run the hub (PAYMUX_LISTEN, default from .env) and the
# public website (HOME_LISTEN) together. Ctrl-C stops both.
HOME_LISTEN ?= 0.0.0.0:8051
dev: SHELL := /bin/bash
dev: build
	@./bin/paymux-home -listen $(HOME_LISTEN) & home=$$!; \
	./bin/paymux & hub=$$!; \
	echo "website: http://$(HOME_LISTEN)"; \
	trap 'kill $$home $$hub 2>/dev/null' INT TERM; \
	wait -n $$home $$hub; status=$$?; \
	kill $$home $$hub 2>/dev/null; wait; exit $$status

run-home:
	$(BUN) run --cwd frontend dev:home

# The hub image (build context = the repository root).
docker:
	docker build -f deploy/docker/Dockerfile --build-arg VERSION=$(VERSION) -t paymux:$(VERSION) -t paymux:latest .

# PKGS narrows the test targets to some packages (scripts/ci.sh test N/M shards the suite).
PKGS ?= ./...

test:
	go test -race -timeout 30m $(PKGS)

test-pg:
	@set -e; \
	if [ -n "$$PAYMUX_TEST_POSTGRES_URL" ]; then \
		PAYMUX_REQUIRE_PG=1 go test -race -timeout 30m $(PKGS); \
	elif command -v docker >/dev/null 2>&1; then \
		cid=$$(docker run -d --rm -e POSTGRES_PASSWORD=paymux -e POSTGRES_DB=paymux -p 127.0.0.1::5432 postgres:17); \
		trap 'docker rm -f $$cid >/dev/null 2>&1' EXIT; \
		port=$$(docker port $$cid 5432/tcp | head -n1 | sed 's/.*://'); \
		for i in $$(seq 1 60); do docker exec $$cid pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done; \
		PAYMUX_TEST_POSTGRES_URL="postgres://postgres:paymux@127.0.0.1:$$port/paymux?sslmode=disable" PAYMUX_REQUIRE_PG=1 go test -race -timeout 30m $(PKGS); \
	else \
		echo "set PAYMUX_TEST_POSTGRES_URL or install docker" >&2; exit 1; \
	fi

test-all: test test-pg

fmt:
	gofmt -w .

ci:
	scripts/ci.sh

run-dev:
	go run ./cmd/paymux

# Stop every running paymux server of this user (matches the process name
# exactly, so it never hits a shell whose command line mentions paymux).
kill:
	@pkill -x -u "$$(id -u)" paymux-home && echo "stopped paymux-home" || true
	@if pkill -x -u "$$(id -u)" paymux; then \
		for i in $$(seq 1 50); do pgrep -x -u "$$(id -u)" paymux >/dev/null || exit 0; sleep 0.1; done; \
		echo "paymux still running after 5s; sending SIGKILL" >&2; pkill -9 -x -u "$$(id -u)" paymux; \
	else echo "no paymux running"; fi

.PHONY: frontend frontend-home frontend-install
frontend-install:
	$(BUN) install --cwd frontend --frozen-lockfile

frontend: frontend-install
	$(BUN) run --cwd frontend typecheck
	$(BUN) run --cwd frontend build:dashboard

frontend-home: frontend-install
	$(BUN) run --cwd frontend typecheck
	$(BUN) run --cwd frontend build:home
