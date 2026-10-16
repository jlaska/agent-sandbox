# agent-run Makefile
# Builds the agent-run binary from Go source

BINARY_NAME := agent-run
VERSION := 1.0.0

# Go parameters
GO := go
GOFLAGS := -v
LDFLAGS := -s -w -X main.version=$(VERSION)

# Pinned to match .github/workflows/test.yml
ACTIONLINT_VERSION := v1.7.12

# Directories
SRC_DIR := ./cmd/agent-run
PKG_DIR := ./internal/agentrun
BUILD_DIR := ./bin

.PHONY: all build clean test lint install setup help images image-claude image-pi image-opencode smoke lint-images lint-ci

# Extra flags for tests/images/smoke.sh (e.g. SMOKE_FLAGS=--skip-network).
# --skip-policy-contract is temporary: sandbox-policy.yaml still lists paths no image provides;
# WP4b removes them and this default (run `make smoke SMOKE_FLAGS=` for the strict check).
SMOKE_FLAGS ?= --skip-policy-contract

# Harnesses covered by `make smoke` (CI passes one, e.g. `make smoke HARNESSES=pi`).
HARNESSES ?= claude pi opencode

## all: Build the binary
all: build

GO_SOURCES := $(shell find $(SRC_DIR) $(PKG_DIR) -name '*.go' 2>/dev/null)

## build: Build the agent-run binary
build: $(BUILD_DIR)/$(BINARY_NAME)

$(BUILD_DIR)/$(BINARY_NAME): $(GO_SOURCES) go.mod
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(SRC_DIR)
	@echo "Binary built: $(BUILD_DIR)/$(BINARY_NAME)"

## test: Run all tests
test:
	@echo "Running tests..."
	$(GO) test $(GOFLAGS) -cover $(PKG_DIR)

## lint: Run linter
lint:
	@echo "Running linter..."
	@which golangci-lint > /dev/null 2>&1 || { echo "golangci-lint not found. Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; exit 1; }
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
