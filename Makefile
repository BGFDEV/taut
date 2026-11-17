SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE  := github.com/bgfdev/taut
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT)
IMAGE   ?= taut:dev
COMPOSE := docker compose -f deploy/compose/compose.yaml
GOLANGCI_VERSION := v2.15.0

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build bin/taut
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/taut ./cmd/taut

.PHONY: test
test: ## Run tests with the race detector
	go test -race ./...

.PHONY: golden
golden: ## Rewrite golden snapshot files
	go test ./... -update

.PHONY: lint
lint: ## Run golangci-lint
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run

.PHONY: fmt
fmt: ## Format code
	gofmt -w cmd internal

.PHONY: fixtures
fixtures: build ## Generate fixture Slack exports into testdata/fixtures
	./bin/taut dev fixtures --out testdata/fixtures

.PHONY: image
image: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .

deploy/compose/.env: | build
	@./scripts/dev-env.sh $@

.PHONY: up
up: deploy/compose/.env ## Start the local stack (Postgres, NATS, object store, collector, fake Graph, the controller, workers)
	@# Build the image first. Most services only name its tag, and Compose may
	@# otherwise create them from a stale image while the build is running.
	$(COMPOSE) build
	$(COMPOSE) up -d --wait --remove-orphans

.PHONY: down
down: ## Stop the local stack and remove its volumes
	$(COMPOSE) down -v --remove-orphans

.PHONY: logs
logs: ## Tail local stack logs
	$(COMPOSE) logs -f

.PHONY: demo
demo: build fixtures ## Run init, auth and preflight against the medium fixture and the fake Graph
	./scripts/demo.sh

.PHONY: local-demo
local-demo: build fixtures ## A whole migration in local mode against the fakes: no Docker, one process (KILL=1 also kills and resumes it)
	TAUT_DEMO_KILL=$(KILL) ./scripts/local-demo.sh

TEST_COMPOSE := docker compose -f deploy/compose/compose.test.yaml

.PHONY: test-services
test-services: ## Start throwaway Postgres, S3 and NATS for integration tests
	$(TEST_COMPOSE) up -d --wait

.PHONY: test-services-down
test-services-down: ## Stop the integration-test services
	$(TEST_COMPOSE) down -v

.PHONY: test-all
test-all: test-services ## Run every test, including those that need Postgres, S3 and NATS
	./scripts/test-all.sh

.PHONY: migrate-demo
migrate-demo: build fixtures ## Import the medium fixture through the running local stack (needs `make up`)
	./scripts/migrate-demo.sh

.PHONY: kill-test
kill-test: build fixtures ## migrate-demo, but kill worker containers mid-run and verify nothing is duplicated
	TAUT_KILL=1 ./scripts/migrate-demo.sh

.PHONY: finalize-demo
finalize-demo: build fixtures ## migrate-demo, then finalize with `taut finalize` and verify the audit trail
	TAUT_FINALIZE=1 ./scripts/migrate-demo.sh

.PHONY: dms-demo
dms-demo: build fixtures ## finalize-demo with direct messages migrated into chats (needs a fresh `make up`)
	TAUT_DMS=1 TAUT_FINALIZE=1 ./scripts/migrate-demo.sh

.PHONY: load-test
load-test: build deploy/compose/.env ## Load-test the local stack: PROFILE=ceiling|throttled|scaling|smoke, WORKERS=2|4|8, SPEC=..., MINUTES=... (docs/LOADTEST.md)
	./scripts/load-test.sh

# --- helm -----------------------------------------------------------------------

HELM_CHART := deploy/helm/taut

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart, render every value set, validate and assert the pod security properties
	$(HELM_CHART)/ci/check.sh

.PHONY: helm-template
helm-template: ## Render the Helm chart with the CI values to stdout
	@helm template taut $(HELM_CHART) --namespace taut -f $(HELM_CHART)/ci/values.yaml

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist
