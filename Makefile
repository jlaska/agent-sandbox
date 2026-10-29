# agent-run Makefile
# Builds the agent-run binary from Go source

BINARY_NAME := agent-run
# Release builds stamp the tag (release.yml); local builds use git describe.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Go parameters
GO := go
GOFLAGS := -v
LDFLAGS := -s -w -X github.com/jlaska/agent-sandbox/internal/agentrun.Version=$(VERSION)

# Pinned to match .github/workflows/test.yml
ACTIONLINT_VERSION := v1.7.12

# Directories
SRC_DIR := ./cmd/agent-run
PKG_DIR := ./internal/agentrun
BUILD_DIR := ./bin

.PHONY: all build clean test lint install setup help images image-claude image-pi image-opencode smoke lint-images lint-ci policy-matrix workflow-gate gate-all

# Extra flags for tests/images/smoke.sh (e.g. SMOKE_FLAGS=--skip-network). The strict
# policy-path contract (every policy binary is in binaries.txt) always runs by default.
SMOKE_FLAGS ?=

# Extra flags for tests/policy-matrix.sh (e.g. MATRIX_FLAGS="--expect hardened").
MATRIX_FLAGS ?=

# Harnesses covered by `make smoke` (CI passes one, e.g. `make smoke HARNESSES=pi`).
HARNESSES ?= claude pi opencode

## all: Build the binary
all: build

## build: Build the agent-run binary
# Always invoke go build: its cache tracks every input (all imported packages,
# embedded files, go.sum, and the VERSION in -ldflags), so a make-level
# prerequisite list would only go stale.
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(SRC_DIR)
	@echo "Binary built: $(BUILD_DIR)/$(BINARY_NAME)"

## test: Run all tests (every package, incl. the embed/contract tests in openshell/)
test:
	@echo "Running tests..."
	$(GO) test $(GOFLAGS) -cover ./...

## lint: Run linter
lint:
	@echo "Running linter..."
	@which golangci-lint > /dev/null 2>&1 || { echo "golangci-lint not found. Install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2"; exit 1; }
	golangci-lint run $(PKG_DIR)

## vet: Run go vet
vet:
	@echo "Running go vet..."
	$(GO) vet $(PKG_DIR)

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)
	$(GO) clean -cache -testcache
	@echo "Clean complete."

## install: Install the binary to GOPATH/bin
install:
	@echo "Installing $(BINARY_NAME)..."
	$(GO) install $(GOFLAGS) -ldflags "$(LDFLAGS)" $(SRC_DIR)
	@echo "Installed to $(shell go env GOPATH)/bin/$(BINARY_NAME)"

## fmt: Format Go source files
fmt:
	@echo "Formatting..."
	$(GO) fmt ./...

## setup: Install pre-commit hooks and development dependencies
setup:
	@echo "Installing development dependencies..."
	@which pre-commit > /dev/null 2>&1 || { echo "Installing pre-commit..."; brew install pre-commit; }
	@which golangci-lint > /dev/null 2>&1 || { echo "Installing golangci-lint..."; brew install golangci-lint; }
	@which shellcheck > /dev/null 2>&1 || { echo "Installing shellcheck..."; brew install shellcheck; }
	@which gitleaks > /dev/null 2>&1 || { echo "Installing gitleaks..."; brew install gitleaks; }
	@echo "Installing pre-commit hooks..."
	pre-commit install
	@echo "Validating hook configuration..."
	pre-commit run --all-files || true
	@echo ""
	@echo "Setup complete. Pre-commit hooks are installed."

## images: Build all harness images locally with podman (localhost/agent-<h>:local)
images:
	./openshell/images/build.sh --local all

## image-claude: Build the claude harness image locally
image-claude:
	./openshell/images/build.sh --local claude

## image-pi: Build the pi harness image locally
image-pi:
	./openshell/images/build.sh --local pi

## image-opencode: Build the opencode harness image locally
image-opencode:
	./openshell/images/build.sh --local opencode

## smoke: Smoke-test all locally built harness images (run make images first)
smoke:
	@for h in $(HARNESSES); do \
		./tests/images/smoke.sh localhost/agent-$$h:local $$h $(SMOKE_FLAGS) || exit 1; \
	done

## policy-matrix: Probe every policy rule in a live sandbox (owner-run; needs a gateway and credentials)
policy-matrix: build
	./tests/policy-matrix.sh $(MATRIX_FLAGS)

# Extra flags for tests/workflow-gate.sh (e.g. GATE_FLAGS="--harness pi").
GATE_FLAGS ?=

## workflow-gate: Run the agent's GitHub workflow end to end in a live session (owner-run)
workflow-gate: build
	./tests/workflow-gate.sh $(GATE_FLAGS)

# Extra flags for tests/gate-all.sh (e.g. GATE_ALL_FLAGS="--pr 119").
GATE_ALL_FLAGS ?=

## gate-all: Run the workflow gate for every harness, or for a PR with --pr N (owner-run)
gate-all:
	./tests/gate-all.sh $(GATE_ALL_FLAGS)

## lint-images: Lint image Dockerfiles (hadolint) and image shell scripts (shellcheck)
lint-images:
	@which hadolint > /dev/null 2>&1 || { echo "hadolint not found. Install: brew install hadolint"; exit 1; }
	@which shellcheck > /dev/null 2>&1 || { echo "shellcheck not found. Install: brew install shellcheck"; exit 1; }
	hadolint openshell/images/*/Dockerfile
	shellcheck openshell/images/build.sh openshell/images/version.sh tests/images/smoke.sh openshell/images/refresh-checksums.sh

## lint-ci: Lint GitHub Actions workflows (actionlint, run via go)
lint-ci:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed 's/^/  /'
