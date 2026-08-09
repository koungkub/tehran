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

K8S_DIR     := deploy/k8s
NAMESPACE   ?= tehran
# Covers an image pull on a cold node. Lower it to see a broken migration fail
# sooner: a failing Job burns this whole budget on its retries before the wait
# gives up.
K8S_TIMEOUT ?= 300s

.PHONY: build test lint docker-build run migrate-new tidy k8s-deploy k8s-diff

build: ## Build static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)

test: ## Run all tests with race detector
	go test -race -cover ./...

lint: ## go vet + golangci-lint (if installed)
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

# The order is the point, and it is why this is a target rather than a line in
# a README that someone runs half of. Applying everything at once would create
# the Job and the Deployment together, and the new pods would race the
# migration they depend on — a race nothing downstream can catch, because
# /readyz checks the database connection and not the schema.
#
# The delete is not optional either: a Job's pod template is immutable, so
# re-applying after a release bump fails with "field is immutable".
k8s-deploy: ## Migrate, wait for it, then roll out (kubectl context must already point at the cluster)
	kubectl delete job tehran-migrate -n $(NAMESPACE) --ignore-not-found
	kubectl apply -k $(K8S_DIR)/migrate
	@# `wait --for=condition=complete` cannot also watch for failure, so a
	@# migration that fails spends the whole timeout retrying and then reports
	@# only "timed out waiting for the condition". The logs are the answer to
	@# the question that message raises, so print them here rather than leave
	@# them to be found.
	@kubectl wait --for=condition=complete job/tehran-migrate -n $(NAMESPACE) --timeout=$(K8S_TIMEOUT) || { \
		echo ""; \
		echo "migration did not complete — not rolling out. Job logs:"; \
		kubectl logs -n $(NAMESPACE) job/tehran-migrate --tail=30 || true; \
		exit 1; \
	}
	kubectl apply -k $(K8S_DIR)
	kubectl rollout status deployment/tehran -n $(NAMESPACE) --timeout=$(K8S_TIMEOUT)

k8s-diff: ## Show what k8s-deploy would change, without changing it
	kubectl diff -k $(K8S_DIR)/migrate || true
	kubectl diff -k $(K8S_DIR) || true
