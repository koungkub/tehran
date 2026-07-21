BINARY  := tehran
MODULE  := github.com/koungkub/tehran
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/platform/version.Version=$(VERSION) \
	-X $(MODULE)/internal/platform/version.GitCommit=$(COMMIT) \
	-X $(MODULE)/internal/platform/version.BuildDate=$(DATE)

.PHONY: generate build test lint docker-build run tidy

generate: ## Regenerate protobuf/Connect code
	buf generate

build: ## Build static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)

test: ## Run all tests with race detector
	go test -race -cover ./...

lint: ## buf lint + go vet + golangci-lint (if installed)
	buf lint
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not installed, skipping"; fi

docker-build: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(BINARY):$(VERSION) .

run: build ## Build and run the API server with the example config
	./bin/$(BINARY) api --config config.toml

tidy:
	go mod tidy
