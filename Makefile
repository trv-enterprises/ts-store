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

.PHONY: all build build-arm64 build-amd64 build-local build-collectors clean test test-verbose help
.PHONY: version-bump release release-binaries release-collectors security-scan security-scan-codeql

# Collectors built and shipped alongside tsstore. Each name must match a
# subdirectory under examples/ that builds as `main`.
#
# NOTE: these are built from the ROOT module (./examples/$c). A collector
# that needs its own go.mod cannot go in this list — see MODULE_COLLECTORS.
COLLECTORS := journal-logs system-stats

# Collectors that are SEPARATE Go modules, built from inside their own
# directory so their dependencies stay out of the root go.mod. synology-snmp
# needs gosnmp for SNMPv3/USM (key derivation, engine discovery, AES-CFB128)
# — protocol crypto that should not be hand-rolled, but also should not be
# imposed on the server module or the stdlib-only collectors.
MODULE_COLLECTORS := synology-snmp

all: build

## Build targets

build: build-arm64 build-amd64 build-collectors ## Build all server + collector binaries for both architectures

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

build-collectors: build-module-collectors ## Build all collectors for both architectures
	@mkdir -p $(BUILD_DIR)
	@for c in $(COLLECTORS); do \
		echo "Building $$c for Linux ARM64..."; \
		GOOS=linux GOARCH=arm64 $(GO) build -ldflags="-s -w -X main.Version=$(VERSION)" -o $(BUILD_DIR)/$$c-linux-arm64 ./examples/$$c; \
		echo "Building $$c for Linux AMD64..."; \
		GOOS=linux GOARCH=amd64 $(GO) build -ldflags="-s -w -X main.Version=$(VERSION)" -o $(BUILD_DIR)/$$c-linux-amd64 ./examples/$$c; \
	done

build-module-collectors: ## Build collectors that are separate Go modules
	@mkdir -p $(BUILD_DIR)
	@for c in $(MODULE_COLLECTORS); do \
		echo "Building $$c (separate module) for Linux ARM64..."; \
		(cd examples/$$c && GOOS=linux GOARCH=arm64 $(GO) build -ldflags="-s -w -X main.Version=$(VERSION)" -o ../../$(BUILD_DIR)/$$c-linux-arm64 .); \
		echo "Building $$c (separate module) for Linux AMD64..."; \
		(cd examples/$$c && GOOS=linux GOARCH=amd64 $(GO) build -ldflags="-s -w -X main.Version=$(VERSION)" -o ../../$(BUILD_DIR)/$$c-linux-amd64 .); \
	done

## Test targets

test: ## Run all tests
	$(GO) test ./...

test-verbose: ## Run all tests with verbose output
	$(GO) test -v ./...

## Security targets

security-scan: ## Reconcile scanner output against security/accepted-vulns.yaml
	@echo "── Security scan ─────────────────────────────────────────────"
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck -json ./... 2>/dev/null | security/reconcile-scan.py --scanner govulncheck; \
		rc=$$?; \
		if [ $$rc -eq 2 ]; then echo "✗ registry error or EXPIRED exception — fix security/accepted-vulns.yaml"; exit 1; fi; \
		if [ $$rc -eq 1 ]; then echo "✗ actionable vulnerabilities — remediate, or accept them in security/accepted-vulns.yaml"; exit 1; fi; \
	else \
		echo "⚠ govulncheck not installed — skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
	fi
	@# CodeQL SARIF is produced by CI, not locally. Pass a downloaded SARIF
	@# with: make security-scan-codeql SARIF=results.sarif
	@echo "✓ security scan complete"

security-scan-codeql: ## Reconcile a CodeQL SARIF file (use with SARIF=path/to/results.sarif)
	@if [ -z "$(SARIF)" ]; then echo "Error: SARIF must be set (e.g. make security-scan-codeql SARIF=results.sarif)"; exit 1; fi
	@security/reconcile-scan.py --scanner codeql --input "$(SARIF)"

## Release targets

version-bump: ## Update version in internal/version/version.go (use with VERSION=vX.Y.Z)
	@if [ "$(VERSION)" = "dev" ] || [ -z "$(VERSION)" ]; then \
		echo "Error: VERSION must be set (e.g., make version-bump VERSION=v0.3.0)"; \
		exit 1; \
	fi
	@echo "Updating internal/version/version.go to $(VERSION)..."
	@sed -i '' 's/Version = "v[^"]*"/Version = "$(VERSION)"/' internal/version/version.go
	@echo "✓ Version updated to $(VERSION)"

release-binaries: build ## Create release binaries (server + collectors) in dist/
	@echo "Creating release binaries for $(VERSION)..."
	@mkdir -p $(DIST_DIR)
	@for arch in $(ARCHS); do \
		echo "  Copying $(BINARY_NAME)-$(VERSION)-$$arch..."; \
		cp $(BUILD_DIR)/$(BINARY_NAME)-$$arch $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)-$$arch; \
	done
	@for c in $(COLLECTORS) $(MODULE_COLLECTORS); do \
		for arch in $(ARCHS); do \
			echo "  Copying $$c-$(VERSION)-$$arch..."; \
			cp $(BUILD_DIR)/$$c-$$arch $(DIST_DIR)/$$c-$(VERSION)-$$arch; \
		done; \
	done
	@echo "✓ Release binaries created:"
	@ls -lh $(DIST_DIR)/*-$(VERSION)-*

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
	git add internal/version/version.go
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
	@echo "  (add --prerelease for rc/beta tags, e.g. v0.12.0-rc.1)"

## Deployment targets (require .env file with PI_HOST, PI_BINARY_PATH, PI_SERVICE)

# deploy-pi installs the new binary BEFORE touching the service:
# install(1) unlinks the target first, so replacing a running binary is safe
# (the process keeps its old inode, no ETXTBSY), and the restart only happens
# once the new binary is fully in place. The previous stop -> cp -> start
# chain needed sudo it didn't have and left the service stopped when the
# copy failed. PI_EXTRA_SERVICES (optional) lists collector units to bounce
# after the restart — they hold connections into tsstore and don't recover
# on their own outside the Ansible-managed flow.
deploy-pi: build-arm64 ## Deploy ARM64 binary to Pi
ifndef PI_HOST
	$(error PI_HOST not set - create .env file or export PI_HOST)
endif
ifndef PI_BINARY_PATH
	$(error PI_BINARY_PATH not set - create .env file or export PI_BINARY_PATH)
endif
ifndef PI_SERVICE
	$(error PI_SERVICE not set - create .env file or export PI_SERVICE)
endif
	@echo "Deploying to $(PI_HOST)..."
	scp $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(PI_HOST):/tmp/$(BINARY_NAME)
	ssh $(PI_HOST) "sudo install -m 755 /tmp/$(BINARY_NAME) $(PI_BINARY_PATH) && sudo systemctl restart $(PI_SERVICE)"
ifdef PI_EXTRA_SERVICES
	ssh $(PI_HOST) "sudo systemctl restart $(PI_EXTRA_SERVICES)"
endif
	@echo "Deployed and restarted: $(PI_SERVICE) $(PI_EXTRA_SERVICES)"

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
