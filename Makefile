# ts-store Makefile
# Build targets for tsstore binaries

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BINARY_NAME = tsstore
BUILD_DIR = bin
DIST_DIR = dist

# Registry for Docker images
REGISTRY := ghcr.io
GITHUB_OWNER ?= $(shell git remote get-url origin | sed -n 's/.*github.com[:/]\([^/]*\)\/.*/\1/p')

# Architectures for release binaries
ARCHS := linux-amd64 linux-arm64

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
.PHONY: version-bump release release-tag

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

## Release targets

version-bump: ## Update version in main.go (use with VERSION=vX.Y.Z)
	@if [ "$(VERSION)" = "dev" ] || [ -z "$(VERSION)" ]; then \
		echo "Error: VERSION must be set (e.g., make version-bump VERSION=v0.3.0)"; \
		exit 1; \
	fi
	@echo "Updating cmd/tsstore/main.go to $(VERSION)..."
	@sed -i '' 's/fmt\.Println("tsstore v[^"]*")/fmt.Println("tsstore $(VERSION)")/' cmd/tsstore/main.go
	@echo "✓ Version updated to $(VERSION)"

release-binaries: build ## Create release binaries in dist/
	@echo "Creating release binaries for $(VERSION)..."
	@mkdir -p $(DIST_DIR)
	@for arch in $(ARCHS); do \
		echo "  Copying $(BINARY_NAME)-$(VERSION)-$$arch..."; \
		cp $(BUILD_DIR)/$(BINARY_NAME)-$$arch $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-$$arch; \
	done
	@echo "✓ Release binaries created:"
	@ls -lh $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-*

release-tag: ## Create git tag (use with VERSION=vX.Y.Z)
	@if [ "$(VERSION)" = "dev" ] || [ -z "$(VERSION)" ]; then \
		echo "Error: VERSION must be set (e.g., make release-tag VERSION=v0.3.0)"; \
		exit 1; \
	fi
	@if git rev-parse "$(VERSION)" >/dev/null 2>&1; then \
		echo "Error: Tag $(VERSION) already exists"; \
		exit 1; \
	fi
	@echo "Creating tag $(VERSION)..."
	git tag -a "$(VERSION)" -m "Release $(VERSION)"
	@echo "✓ Tag $(VERSION) created"

release: ## Full release: bump version, build, commit, tag, push (use with VERSION=vX.Y.Z)
	@if [ "$(VERSION)" = "dev" ] || [ -z "$(VERSION)" ]; then \
		echo "Error: VERSION must be set"; \
		echo "Usage: make release VERSION=v0.3.0"; \
		exit 1; \
	fi
	@if git rev-parse "$(VERSION)" >/dev/null 2>&1; then \
		echo "Error: Tag $(VERSION) already exists"; \
		exit 1; \
	fi
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "Error: You have uncommitted changes. Commit or stash them first."; \
		git status --short; \
		exit 1; \
	fi
	@echo "============================================"
	@echo "Starting release $(VERSION)"
	@echo "============================================"
	@$(MAKE) version-bump VERSION=$(VERSION)
	@$(MAKE) build VERSION=$(VERSION)
	@$(MAKE) release-binaries VERSION=$(VERSION)
	@echo ""
	@echo "Committing version bump..."
	git add cmd/tsstore/main.go
	git commit -m "Bump version to $(VERSION)"
	@echo ""
	@echo "Creating tag $(VERSION)..."
	git tag -a "$(VERSION)" -m "Release $(VERSION)"
	@echo ""
	@echo "Pushing to origin..."
	git push origin main
	git push origin "$(VERSION)"
	@echo ""
	@echo "============================================"
	@echo "Release $(VERSION) complete!"
	@echo "============================================"
	@echo ""
	@echo "Binaries:"
	@ls $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-* 2>/dev/null | sed 's/^/  /'
	@echo ""
	@echo "GitHub Actions is now publishing Docker image to:"
	@echo "  $(REGISTRY)/$(GITHUB_OWNER)/ts-store:$(VERSION)"
	@echo ""
	@echo "Create GitHub release with binaries (optional):"
	@echo "  gh release create $(VERSION) $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-*"

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
	rm -rf $(DIST_DIR)

help: ## Show this help
	@echo "ts-store Makefile targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Release workflow:"
	@echo "  make release VERSION=v0.3.1"
	@echo ""
	@echo "Current version: $(VERSION)"
	@echo "Registry: $(REGISTRY)/$(GITHUB_OWNER)"
	@echo ""
	@echo "Environment variables (set in .env or export):"
	@echo "  VERSION        - Version tag (default: git describe)"
	@echo "  PI_HOST        - SSH target for deploy-pi (e.g., user@host)"
	@echo "  PI_BINARY_PATH - Remote path for binary"
	@echo "  PI_SERVICE     - Systemd service name"
