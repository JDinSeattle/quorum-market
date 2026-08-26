# Quorum Market — common tasks. Everything works from the repository root.

COMPOSE := docker compose -f deploy/docker-compose.yml
GO      ?= go

# Version metadata, stamped into the binaries and the image so a running
# container can say exactly what it is.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE      ?= quorum-market:$(VERSION)

# Host ports for the observability profile. Overridable because 3000 and the
# 909x range are crowded on a developer machine.
PROMETHEUS_PORT ?= 9490
GRAFANA_PORT    ?= 3000

LDFLAGS := -s -w \
	-X github.com/JDinSeattle/quorum-market/internal/obs.version=$(VERSION) \
	-X github.com/JDinSeattle/quorum-market/internal/obs.commit=$(COMMIT) \
	-X github.com/JDinSeattle/quorum-market/internal/obs.date=$(BUILD_DATE)

export VERSION COMMIT BUILD_DATE PROMETHEUS_PORT GRAFANA_PORT

.DEFAULT_GOAL := help

## help: list the available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'

# ── Build ────────────────────────────────────────────────────────────────────

## build: compile every service binary into ./bin
build:
	$(GO) build -trimpath -ldflags='$(LDFLAGS)' -o bin/ ./cmd/...

## image: build the container image holding every service
image:
	docker build -f deploy/Dockerfile -t $(IMAGE) -t quorum-market:local \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

# ── Quality gates ────────────────────────────────────────────────────────────

## test: run the test suite with the race detector
test:
	$(GO) test ./... -race -count=1

## test-short: run the test suite without the race detector
test-short:
	$(GO) test ./... -count=1

## cover: run the tests and open a coverage report
cover:
	$(GO) test ./... -coverprofile=coverage.out -covermode=atomic
	$(GO) tool cover -func=coverage.out | tail -1
	$(GO) tool cover -html=coverage.out

## lint: run golangci-lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 \
		|| { echo "install it: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run ./...

## fmt: format Go sources and imports
fmt:
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint fmt ./... || gofmt -w ./cmd ./internal

## vuln: check dependencies for known vulnerabilities
vuln:
	@command -v govulncheck >/dev/null 2>&1 \
		|| { echo "install it: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck ./...

## tidy: prune and verify module requirements
tidy:
	$(GO) mod tidy

## verify: everything CI runs, locally
verify: tidy lint vuln test tf-check
	@echo "\033[32mall checks passed\033[0m"

# ── Local stack ──────────────────────────────────────────────────────────────

## up: build and start the whole stack, waiting until it is healthy
up:
	$(COMPOSE) up --build -d --wait --wait-timeout 300

## down: stop the stack and remove its volumes
down:
	$(COMPOSE) down -v --remove-orphans

## restart: rebuild and restart the application services only
restart:
	$(COMPOSE) up --build -d --no-deps --wait \
		product-service shopping-cart-service warehouse-service cca-service

## logs: follow logs from every container
logs:
	$(COMPOSE) logs -f

## ps: show the state of the stack
ps:
	$(COMPOSE) ps

## seed: fill the catalogue with products (PRODUCT_COUNT=1000)
seed:
	$(COMPOSE) --profile seed run --rm product-loader

## smoke: exercise the running stack end to end
smoke:
	./scripts/smoke.sh
	./scripts/smoke-platform.sh

## smoke-core: the commerce path only, against the services directly
smoke-core:
	./scripts/smoke.sh

## smoke-platform: the gateway, auth, cache and event-driven services
smoke-platform:
	./scripts/smoke-platform.sh

## observability: start Prometheus and Grafana against the running stack
observability:
	# Named explicitly so this cannot recreate the running stack underneath
	# itself: a plain 'up' with the profile re-evaluates every service and will
	# happily restart the system you were about to observe.
	$(COMPOSE) --profile observability up -d --wait prometheus grafana
	@echo "  Grafana     http://localhost:$(GRAFANA_PORT)  (dashboard: Quorum Market)"
	@echo "  Prometheus  http://localhost:$(PROMETHEUS_PORT)"

## metrics: print the cart service's current metrics
metrics:
	@curl -s http://localhost:9213/metrics | grep -E '^quorum_' | grep -v '_bucket' | head -40

## ready: show every service's readiness report
ready:
	@for port in 9210 9211 9212 9213; do \
		printf '%s: ' $$port; curl -s --max-time 2 http://localhost:$$port/readyz | head -c 400; echo; \
	done

## loadtest: run Locust against the local stack
loadtest:
	locust -f loadtest/locustfile.py --host http://localhost:8082

# ── Infrastructure ───────────────────────────────────────────────────────────

## tf-check: format and validate the infrastructure configuration
tf-check:
	@tf=$$(command -v terraform || command -v tofu) || { echo "install terraform or tofu"; exit 1; }; \
	cd terraform && $$tf fmt -recursive -check && $$tf init -backend=false >/dev/null && $$tf validate

## evidence: run every gate and record the results under evidence/
evidence:
	./scripts/collect-evidence.sh

## clean: remove build output
clean:
	rm -rf bin coverage.out

.PHONY: help build image test test-short cover lint fmt vuln tidy verify evidence \
        up down restart logs ps seed smoke smoke-core smoke-platform observability metrics ready loadtest tf-check clean
