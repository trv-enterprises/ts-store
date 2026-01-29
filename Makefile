# ts-store Makefile
# Build targets for tsstore binaries

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BINARY_NAME = tsstore
BUILD_DIR = bin

# Load .env file if it exists (for deployment settings)
-include .env
export

# Go settings (use homebrew Go on macOS if available)
ifeq ($(shell uname -s),Darwin)
  ifeq ($(shell [ -d /opt/homebrew/opt/go/libexec ] && echo yes),yes)
    export GOROOT = /opt/homebrew/opt/go/libexec
  endif
endif
GO = go

# Build flags
LDFLAGS = -s -w -X main.Version=$(VERSION)

.PHONY: all build build-arm64 build-amd64 build-local clean test test-verbose help

all: build

## Build targets

build: build-arm64 build-amd64 ## Build both ARM64 and AMD64 binaries

build-arm64: ## Build Linux ARM64 binary
	@echo "Building $(BINARY_NAME) for Linux ARM64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/tsstore

build-amd64: ## Build Linux AMD64 binary
	@echo "Building $(BINARY_NAME) for Linux AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/tsstore

build-local: ## Build for local architecture
	@echo "Building $(BINARY_NAME) for local system..."
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/tsstore

## Test targets

test: ## Run all tests
	$(GO) test ./...

test-verbose: ## Run all tests with verbose output
	$(GO) test -v ./...

## Deployment targets (require .env file with PI_HOST, PI_BINARY_PATH, PI_SERVICE)

deploy-pi: build-arm64 ## Deploy ARM64 binary to Pi
ifndef PI_HOST
	$(error PI_HOST not set - create .env file or export PI_HOST)
endif
ifndef PI_BINARY_PATH
	$(error PI_BINARY_PATH not set - create .env file or export PI_BINARY_PATH)
endif
	@echo "Deploying to $(PI_HOST)..."
	scp $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(PI_HOST):/tmp/$(BINARY_NAME)
	ssh $(PI_HOST) "sudo systemctl stop $(PI_SERVICE) && cp /tmp/$(BINARY_NAME) $(PI_BINARY_PATH) && chmod +x $(PI_BINARY_PATH) && sudo systemctl start $(PI_SERVICE)"
	@echo "Deployed and restarted $(PI_SERVICE)"

## Utility targets

clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)

help: ## Show this help
	@echo "ts-store Makefile targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Environment variables (set in .env or export):"
	@echo "  VERSION        - Version tag (default: git describe)"
	@echo "  PI_HOST        - SSH target for deploy-pi (e.g., user@host)"
	@echo "  PI_BINARY_PATH - Remote path for binary"
	@echo "  PI_SERVICE     - Systemd service name"
