# ts-store Project Instructions

## Release Process

Before building binaries for a new release:

1. **Update the version number** in `cmd/tsstore/main.go` (search for `fmt.Println("tsstore v`)
2. Commit the version bump
3. Create and push the git tag
4. Build binaries: `GOOS=linux GOARCH=amd64 go build -o tsstore-linux-amd64 ./cmd/tsstore`
5. Create the GitHub release and upload binaries

## Build Commands

```bash
# Linux AMD64
GOOS=linux GOARCH=amd64 go build -o tsstore-linux-amd64 ./cmd/tsstore

# Linux ARM64
GOOS=linux GOARCH=arm64 go build -o tsstore-linux-arm64 ./cmd/tsstore
```
