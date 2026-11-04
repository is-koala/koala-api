# Quality gate and developer entry points. `make verify` is the gate: a PR
# is opened only when it is green (CLAUDE.md, ADR-0004).

GO ?= go
BIN := $(CURDIR)/bin

# golangci-lint is built from source at a pinned version into ./bin, with the
# repo's own Go toolchain (ADR-0004). Bump together with the go directive.
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: verify
verify: fmt-check mod-check vet lint test ## The quality gate: gofmt, go.mod tidy, vet, golangci-lint, race tests

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt'ed
	@out="$$(gofmt -l $$(git ls-files --cached --others --exclude-standard '*.go'))"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: mod-check
mod-check: ## Fail if go.mod/go.sum are not tidy (no unused or missing dependencies)
	$(GO) mod tidy -diff

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## golangci-lint (pinned)
	$(GOLANGCI_LINT) run

.PHONY: test
test: ## Race-enabled tests, no cache
	$(GO) test -race -count=1 ./...

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Rewrite files with gofmt and goimports
	$(GOLANGCI_LINT) fmt

.PHONY: build
build: ## Build ./bin/koala
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN)/koala ./cmd/koala

.PHONY: run
run: ## Run the server locally (KOALA_ENV defaults to local)
	KOALA_ENV=$${KOALA_ENV:-local} $(GO) run ./cmd/koala serve

.PHONY: tools
tools: $(GOLANGCI_LINT) ## Install pinned tools into ./bin

$(GOLANGCI_LINT):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) GOTOOLCHAIN=local $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(BIN)/golangci-lint $@

.PHONY: clean
clean: ## Remove build outputs and tools
	rm -rf $(BIN)
