# GitSafe developer tasks. Works with GNU make on Linux and macOS.
# Windows: see CONTRIBUTING.md for the equivalent go commands.

GO            ?= go
BINARY        ?= bin/gitsafe
IMAGE         ?= gitsafe:dev
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null)
# Matches the "db" service in docker-compose.yml.
TEST_DATABASE_URL ?= postgresql://gitsafe:gitsafe@127.0.0.1:5432/gitsafe?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help run build test race lint fmt docker db-up db-down test-postgres

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

run: ## Run the server locally (reads .env)
	$(GO) run ./cmd/server

build: ## Build the server binary into bin/
	$(GO) build -o $(BINARY) ./cmd/server

test: ## Run unit tests
	$(GO) test -count=1 ./...

race: ## Run unit tests with the race detector
	$(GO) test -count=1 -race ./...

lint: ## gofmt check, go vet, and golangci-lint (if installed)
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) vet ./...
	$(GO) vet -tags=postgres ./...
ifneq ($(GOLANGCI_LINT),)
	$(GOLANGCI_LINT) run ./...
else
	@echo "golangci-lint not installed; skipping (CI runs it). See https://golangci-lint.run/welcome/install/"
endif

fmt: ## Format all Go files
	gofmt -w .

docker: ## Build the production Docker image
	docker build -t $(IMAGE) .

db-up: ## Start the local PostgreSQL from docker-compose.yml
	docker compose up -d db

db-down: ## Stop the local PostgreSQL (data is kept in a volume)
	docker compose down

test-postgres: ## Run PostgreSQL integration tests (needs 'make db-up')
	GITSAFE_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test -tags=postgres -count=1 -p 1 ./...
