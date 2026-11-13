# Development tasks. Each target is one command you can also run without make.

GO_IMAGE   ?= golang:1.26.5
LINT_IMAGE ?= golangci/golangci-lint:v2.15.0

# Stops Git Bash on Windows from rewriting container paths such as /src.
export MSYS_NO_PATHCONV := 1

DOCKER_RUN = docker run --rm -v "$(CURDIR)":/src -w /src

.PHONY: help up down build vet test test-race lint fmt

help: ## Show this help
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-18s %s\n", $$1, $$2}'

up: ## Start the stack (migrations and seed run automatically)
	docker compose up -d

down: ## Stop the stack and keep the data
	docker compose down

build: ## Compile all packages
	go build ./...

vet: ## Run go vet
	go vet ./...

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector (in Docker, needs cgo)
	$(DOCKER_RUN) -v gomodcache:/go/pkg/mod $(GO_IMAGE) go test -race ./...

lint: ## Run golangci-lint (in Docker)
	$(DOCKER_RUN) -v golangci-cache:/root/.cache $(LINT_IMAGE) golangci-lint run

fmt: ## Format code with gofmt and goimports (in Docker)
	$(DOCKER_RUN) -v golangci-cache:/root/.cache $(LINT_IMAGE) golangci-lint fmt
