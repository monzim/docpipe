.PHONY: help build run test vet fmt lint docker docker-up docker-down soak openapi-validate clean

GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    = -s -w \
             -X main.version=$(VERSION) \
             -X main.commit=$(COMMIT) \
             -X main.buildDate=$(BUILD_DATE)

help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the docpipe binary into ./bin/docpipe
	@mkdir -p bin
	$(GO) build -trimpath -ldflags='$(LDFLAGS)' -o bin/docpipe ./cmd/docpipe

run: build ## Build and run with env loaded from .env if present
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; ./bin/docpipe

test: ## Run unit tests
	$(GO) test ./...

test-integration: ## Run integration tests (requires chromium-browser in PATH)
	$(GO) test -tags=integration ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Run gofmt and goimports
	gofmt -w .

lint: vet ## Vet today; add golangci-lint later if desired
	@if command -v golangci-lint >/dev/null; then golangci-lint run; else echo "golangci-lint not installed, skipping"; fi

docker: ## Build the Docker image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-f deploy/Dockerfile \
		-t docpipe:$(VERSION) -t docpipe:latest .

docker-up: ## docker compose up --build
	docker compose -f deploy/docker-compose.yml up --build

docker-down: ## docker compose down
	docker compose -f deploy/docker-compose.yml down

soak: ## Run the soak test against a docker-compose instance
	./scripts/soak.sh

openapi-validate: ## Validate the OpenAPI spec using spectral if installed
	@if command -v spectral >/dev/null; then spectral lint internal/webassets/openapi.yaml; else echo "spectral not installed; skipping"; fi

clean: ## Remove built artifacts
	rm -rf bin/ dist/
