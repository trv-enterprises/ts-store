# ts-store Project Instructions

## Release Process

Use the Makefile for releases:

```bash
make release VERSION=v0.3.1
```

This will:
1. Update the version in `cmd/tsstore/main.go`
2. Build Linux binaries (amd64 + arm64)
3. Commit the version bump
4. Create and push the git tag
5. Push to origin (triggers Docker image build via GitHub Actions)

The Docker image is automatically published to `ghcr.io/trv-enterprises/ts-store`.

To create a GitHub release with binaries:
```bash
gh release create v0.3.1 dist/tsstore-v0.3.1-*
```

## Build Commands

```bash
# Build both architectures
make build

# Build for local development
make build-local

# Run tests
make test

# See all targets
make help
```

## Deploy to Pi

```bash
# Requires .env with PI_HOST, PI_BINARY_PATH, PI_SERVICE
make deploy-pi
```
