# Quality gate and developer entry points. `make verify` is the gate. It runs
# on GitHub's runners (.github/workflows/verify.yml); on the shared Mac only
# the light targets (fmt-check, mod-check, fmt) are run (CLAUDE.md, ADR-0004).

GO ?= go
BIN := $(CURDIR)/bin

# Tools are built from source at pinned versions into ./bin (ADR-0004).
# golangci-lint must support the Go version in go.mod; bump them together.
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GOVULNCHECK_VERSION := v1.9.0
GOVULNCHECK := $(BIN)/govulncheck-$(GOVULNCHECK_VERSION)
# OpenAPI lint (ADR-0019). oapi-codegen is pinned in
# internal/apiv1/generate.go, where `go generate` reads it.
VACUUM_VERSION := v0.36.2
VACUUM := $(BIN)/vacuum-$(VACUUM_VERSION)
# sqlc (ADR-0020): built with CGO_ENABLED=0, so its Postgres parser is
# libpg_query compiled to WebAssembly and no C toolchain is needed; the
# same binary results on the Mac and on the runner. Like the linters it
# stays out of go.mod (ADR-0004): its module graph (MySQL, SQLite, gRPC,
# lib/pq …) is not the application's.
SQLC_VERSION := v1.31.1
SQLC := $(BIN)/sqlc-$(SQLC_VERSION)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: verify
verify: fmt-check mod-check vet lint lint-probe vuln test ## The quality gate (CI): gofmt, go.mod tidy, vet, golangci-lint, lint probe, govulncheck, race tests

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

.PHONY: lint-probe
lint-probe: $(GOLANGCI_LINT) ## Prove the depguard/forbidigo/sloglint rules fire (generated, uncommitted probe)
	scripts/lint-probe.sh $(GOLANGCI_LINT)

.PHONY: vuln
vuln: $(GOVULNCHECK) ## govulncheck against the Go vulnerability database
	$(GOVULNCHECK) ./...

.PHONY: test
test: ## Race-enabled tests in random order, no cache
	# -v names every test in the CI log: without it the log holds one line
	# per package, so a review cannot tell which tests ran at all. 284 test
	# functions, so the output stays in the low thousands of lines.
	$(GO) test -race -shuffle=on -count=1 -v ./...

.PHONY: test-integration
test-integration: ## Integration tests only (build tag integration, tag guards first) against PGHOST/PGPORT/PGUSER; CI only
	scripts/test-integration.sh

.PHONY: test-conformance
test-conformance: ## Authz conformance (ADR-0024): matrix rows on the Go evaluator and on OpenFGA at FGA_API_URL; CI only
	@for f in TestConformanceWithOpenFGA TestConformanceFolderModel; do \
		grep -q "^func $$f(" internal/authz/openfga_conformance_test.go || { echo "test-conformance: $$f not found" >&2; exit 1; }; \
	done
	$(GO) test -tags conformance -race -count=1 -v -run '^(TestConformanceWithOpenFGA|TestConformanceFolderModel)$$' ./internal/authz/

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
tools: $(GOLANGCI_LINT) $(GOVULNCHECK) $(VACUUM) $(SQLC) ## Install pinned tools into ./bin

$(GOLANGCI_LINT):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(BIN)/golangci-lint $@

$(GOVULNCHECK):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	mv $(BIN)/govulncheck $@

# The API contract checks join the gate. They are added on their own line,
# not in the list above, so the two lines change independently.
verify: api-lint gen-check

# The per-rule wall-clock budget vacuum gets for each file. Its default is
# 5s, and the operation-level rules (one JSON-schema check per operation)
# outgrew it when the Faz 2 contract reached 88 operations (ADR-0029).
# Running out of the budget is never silent: scripts/api-lint.sh fails the
# step when vacuum reports that it skipped a rule, because a skipped rule
# reports nothing and so looks exactly like a passing one. The budget is
# per rule and per file; a rule that needs more than a minute on one file
# is pathological and should be seen, not hidden (ADR-0027's ReDoS lesson).
VACUUM_TIMEOUT := 60

.PHONY: api-lint
api-lint: $(VACUUM) ## Lint api/ (vacuum, api/ruleset*.yaml) and prove every rule fires
	scripts/api-lint.sh $(VACUUM) $(VACUUM_TIMEOUT)
	scripts/api-lint-probe.sh $(VACUUM) $(VACUUM_TIMEOUT)

# Packages generated by oapi-codegen: the /v1 server and the generator's
# OpenAPI 3.1 regression test.
GENERATED := internal/apiv1 internal/codegentest
# Packages generated by sqlc: every out directory of sqlc.yaml. They hold
# generated files only and are emptied before each run.
SQLC_OUT := $(shell awk '/^ *out:/ {print $$2}' sqlc.yaml)
# Files softdeletegen writes into: migration directives and the
# no-base-table rule (ADR-0020).
SOFTDELETE_GENERATED := db/migrations internal/platform/dbtest/testdata/migrations sqlc.yaml
# Files docs/faz0/tarama/pairs.py writes: the authz conformance matrix and
# the Faz 0 pair table (ADR-0024, ADR-0028). They are generated like the
# rest, so a stale matrix is red instead of quietly producing the wrong
# evidence in the authz-conformance job.
PAIRS := docs/faz0/tarama/pairs.py
PAIRS_GENERATED := internal/authz/testdata/matrix.yaml docs/faz0/authz-ciftleri.md
ALL_GENERATED := $(GENERATED) $(SQLC_OUT) $(SOFTDELETE_GENERATED) $(PAIRS_GENERATED)

# python3 is on the runner image (ubuntu-latest) and on the Mac; it is not
# pinned, because the script is plain stdlib and writes its output
# deterministically. -I keeps it from importing anything next to it.
PYTHON ?= python3

.PHONY: generate
generate: $(SQLC) ## Regenerate soft-delete SQL blocks, the authz matrix, sqlc and oapi-codegen packages; stale outputs are removed first
	$(GO) run ./internal/tools/softdeletegen
	@command -v $(PYTHON) >/dev/null || { echo "generate: $(PYTHON) is needed for $(PAIRS)" >&2; exit 1; }
	$(PYTHON) -I $(PAIRS)
	rm -f $(addsuffix /*.go,$(SQLC_OUT))
	$(SQLC) generate
	rm -f $(addsuffix /*.gen.go,$(GENERATED))
	$(GO) generate $(addprefix ./,$(GENERATED))

.PHONY: gen-check
gen-check: generate ## Fail if the committed generated code is not what the sources generate
	@if [ -n "$$(git status --porcelain -- $(ALL_GENERATED))" ]; then \
		echo "generated code is stale: run 'make generate' and commit the result"; \
		git status --porcelain -- $(ALL_GENERATED); git diff -- $(ALL_GENERATED); exit 1; \
	fi

# sqlc vet joins the gate on its own line, like the contract checks.
verify: sqlc-vet

.PHONY: sqlc-vet
sqlc-vet: $(SQLC) ## sqlc vet (no-base-table rule) and the probe proving the rule fires
	$(SQLC) vet
	scripts/sqlc-vet-probe.sh $(SQLC)

$(SQLC):
	@mkdir -p $(BIN)
	CGO_ENABLED=0 GOBIN=$(BIN) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	mv $(BIN)/sqlc $@

$(VACUUM):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) $(GO) install github.com/daveshanley/vacuum@$(VACUUM_VERSION)
	mv $(BIN)/vacuum $@

.PHONY: clean
clean: ## Remove build outputs and tools
	rm -rf $(BIN)
