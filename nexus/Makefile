.PHONY: all build test lint clean run-infra stop-infra

# Go settings
GO := go
GOFLAGS := -v
BUILD_DIR := bin

all: lint test build

## Build
build:
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/nexus-core ./cmd/nexus-core
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/nexus-intel ./cmd/nexus-intel
	@echo "Build complete: $(BUILD_DIR)/"

## Test
test:
	$(GO) test ./... -race -count=1 -timeout 60s

test-verbose:
	$(GO) test ./... -race -count=1 -timeout 60s -v

test-coverage:
	$(GO) test ./... -race -coverprofile=coverage.out -timeout 60s
	$(GO) tool cover -html=coverage.out -o coverage.html

## Lint
lint:
	$(GO) vet ./...
	@echo "Lint passed"

## Clean
clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html

## Infrastructure (Docker)
run-infra:
	docker compose -f deploy/docker-compose.yml up -d
	@echo "Infrastructure started. Waiting for services..."
	@sleep 5
	@echo "RedPanda: localhost:19092"
	@echo "ClickHouse: localhost:9000 (native) / localhost:8123 (HTTP)"
	@echo "Prometheus: localhost:9090"
	@echo "Grafana: localhost:3000"

stop-infra:
	docker compose -f deploy/docker-compose.yml down

## Topic provisioning
provision-topics:
	@echo "Provisioning Kafka topics..."
	./scripts/provision-topics.sh

## Run
run-core: build
	./$(BUILD_DIR)/nexus-core

run-intel: build
	./$(BUILD_DIR)/nexus-intel

## Help
help:
	@echo "NEXUS Trading Platform"
	@echo ""
	@echo "Usage:"
	@echo "  make build          - Build all binaries"
	@echo "  make test           - Run tests"
	@echo "  make lint           - Run linter"
	@echo "  make run-infra      - Start infrastructure (RedPanda, ClickHouse, etc.)"
	@echo "  make stop-infra     - Stop infrastructure"
	@echo "  make provision-topics - Create Kafka topics"
	@echo "  make run-core       - Build and run trading core"
	@echo "  make run-intel      - Build and run intel pipeline"
