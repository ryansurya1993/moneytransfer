# Development tasks. Each target is one command you can also run without make.

GO_IMAGE   ?= golang:1.26.5
LINT_IMAGE ?= golangci/golangci-lint:v2.15.0
WORKERS    ?= 1
AMOUNT     ?= 1000000000

# Stops Git Bash on Windows from rewriting container paths such as /src.
export MSYS_NO_PATHCONV := 1

DOCKER_RUN = docker run --rm -v "$(CURDIR)":/src -w /src

.PHONY: help up down build vet test test-integration test-race lint fmt loadtest topup

help: ## Show this help
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-18s %s\n", $$1, $$2}'

up: ## Build and start the stack and wait until it is healthy (migrations and seed run automatically)
	docker compose up -d --build --wait

down: ## Stop the stack and keep the data
	docker compose down

build: ## Compile all packages
	go build ./...

vet: ## Run go vet
	go vet ./...

test: ## Run unit tests
	go test ./...

# One package at a time: test packages share one testcontainers cleanup container, and a package
# that finds it still starting fails instead of waiting.
test-integration: ## Run integration tests against real Postgres and RabbitMQ (needs Docker)
	go test -tags=integration -p 1 ./...

test-race: ## Run unit tests with the race detector (in Docker, needs cgo)
	$(DOCKER_RUN) -v gomodcache:/go/pkg/mod $(GO_IMAGE) go test -race ./...

lint: ## Run golangci-lint (in Docker)
	$(DOCKER_RUN) -v golangci-cache:/root/.cache $(LINT_IMAGE) golangci-lint run

fmt: ## Format code with gofmt and goimports (in Docker)
	$(DOCKER_RUN) -v golangci-cache:/root/.cache $(LINT_IMAGE) golangci-lint fmt

# Without --no-deps, `run` would put the worker count back to the one in docker-compose.yml.
loadtest: ## Send a burst of transfers and check the money afterwards (WORKERS=3 for more workers)
	docker compose up -d --build --wait --scale worker=$(WORKERS) && docker compose run --rm --build --no-deps loadtest

topup: ## Add AMOUNT whole rupiah to the merchant balance, with a DEPOSIT ledger entry (needs the stack)
	docker compose run --rm --build --no-deps loadtest topup $(AMOUNT)
