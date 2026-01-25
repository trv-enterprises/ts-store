# ts-store Makefile
# Build and publish targets for tsstore binaries

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BINARY_NAME = tsstore
BUILD_DIR = bin

# Go settings (use homebrew Go on macOS if available)
ifeq ($(shell uname -s),Darwin)
  ifeq ($(shell [ -d /opt/homebrew/opt/go/libexec ] && echo yes),yes)
    export GOROOT = /opt/homebrew/opt/go/libexec
  endif
endif
GO = go

# Artifactory settings
ARTIFACTORY_URL = http://100.127.19.27:8081/artifactory
ARTIFACTORY_REPO = trve-repo-local
ARTIFACTORY_PATH = ts-store
ARTIFACTORY_USER ?= admin
ARTIFACTORY_PASS ?= $(error ARTIFACTORY_PASS not set - export it or pass via make ARTIFACTORY_PASS=xxx)

# Build flags
LDFLAGS = -s -w -X main.Version=$(VERSION)

.PHONY: all build build-arm64 build-amd64 clean test publish publish-arm64 publish-amd64 help

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

## Publish targets

publish: build publish-arm64 publish-amd64 ## Build and publish both binaries to Artifactory

publish-arm64: ## Publish ARM64 binary to Artifactory
	@echo "Publishing $(BINARY_NAME)-linux-arm64 to Artifactory..."
	@curl -u $(ARTIFACTORY_USER):$(ARTIFACTORY_PASS) \
		-T $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 \
		"$(ARTIFACTORY_URL)/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)/latest-arm64/$(BINARY_NAME)"
	@echo ""
	@echo "Published to: $(ARTIFACTORY_URL)/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)/latest-arm64/$(BINARY_NAME)"

publish-amd64: ## Publish AMD64 binary to Artifactory
	@echo "Publishing $(BINARY_NAME)-linux-amd64 to Artifactory..."
	@curl -u $(ARTIFACTORY_USER):$(ARTIFACTORY_PASS) \
		-T $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 \
		"$(ARTIFACTORY_URL)/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)/latest-amd64/$(BINARY_NAME)"
	@echo ""
	@echo "Published to: $(ARTIFACTORY_URL)/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)/latest-amd64/$(BINARY_NAME)"

publish-versioned: build ## Publish both binaries with version tag
	@echo "Publishing version $(VERSION) to Artifactory..."
	@curl -u $(ARTIFACTORY_USER):$(ARTIFACTORY_PASS) \
		-T $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 \
		"$(ARTIFACTORY_URL)/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)/$(VERSION)/$(BINARY_NAME)-linux-arm64"
	@curl -u $(ARTIFACTORY_USER):$(ARTIFACTORY_PASS) \
		-T $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 \
		"$(ARTIFACTORY_URL)/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)/$(VERSION)/$(BINARY_NAME)-linux-amd64"
	@echo ""
	@echo "Published to: $(ARTIFACTORY_URL)/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)/$(VERSION)/"

## Utility targets

clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)

list-artifacts: ## List artifacts in Artifactory
	@echo "ts-store artifacts in Artifactory:"
	@echo ""
	@curl -s -u $(ARTIFACTORY_USER):$(ARTIFACTORY_PASS) \
		"$(ARTIFACTORY_URL)/api/storage/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)" | \
		jq -r '.children[]?.uri // empty' 2>/dev/null | while read dir; do \
			echo "$$dir:"; \
			curl -s -u $(ARTIFACTORY_USER):$(ARTIFACTORY_PASS) \
				"$(ARTIFACTORY_URL)/api/storage/$(ARTIFACTORY_REPO)/$(ARTIFACTORY_PATH)$$dir" | \
				jq -r '.children[]? | "  \(.uri) (\(.folder // false | if . then "folder" else "file" end))"' 2>/dev/null; \
		done

help: ## Show this help
	@echo "ts-store Makefile targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Environment variables:"
	@echo "  VERSION          - Version tag (default: git describe)"
	@echo "  ARTIFACTORY_USER - Artifactory username (default: admin)"
	@echo "  ARTIFACTORY_PASS - Artifactory password (default: password)"
