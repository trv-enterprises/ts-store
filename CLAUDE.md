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
Linux binaries are attached to the GitHub Release by the `release-binaries.yml` workflow on tag push.

### Release notes

**Claude writes release notes by hand for every tagged release.** The `release-binaries.yml` workflow auto-creates the GitHub Release with a bare commit-list body — that body is a placeholder and must be replaced. See [RELEASING.md](./RELEASING.md) for the structure (Highlights / Breaking changes / Compatibility), title format, and the `gh release edit` invocation. After any `make release`, draft notes from `git log <prev-tag>..<new-tag>` and post them to the release page. If a prior release shipped with only the auto-generated body, offer to backfill it.

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
