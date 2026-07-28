BINARY  := tehran
MODULE  := github.com/koungkub/tehran
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(DATE)

GOOSE   := github.com/pressly/goose/v3/cmd/goose@v3.27.3

.PHONY: generate build test lint docker-build run migrate-new tidy

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

# The goose CLI is used for this and nothing else: creating the file is the one
# part of the workflow that has to write to the source tree, which is not
# something the shipped binary should be able to do. Applying migrations is
# `tehran db migrate`, which reads them from its own embedded copy.
#
# Timestamped, not sequential. Two branches each adding "the next" number merge
# cleanly and then collide at run time, where the collision is nobody's review
# comment.
migrate-new: ## Create a timestamped SQL migration: make migrate-new NAME=create_accounts
	@test -n "$(NAME)" || { echo "usage: make migrate-new NAME=create_accounts"; exit 1; }
	go run $(GOOSE) -dir internal/migrations create $(NAME) sql

tidy:
	go mod tidy
