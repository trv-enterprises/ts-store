# ts-store CLI Reference

[Back to main README](README.md)

This document covers command-line operations for ts-store.

## Installation

```bash
go get github.com/tviviano/ts-store
```

### Build the Server

```bash
go build -o tsstore ./cmd/tsstore
```

## Docker

Build and run with Docker:

```bash
# Build the image
docker build -t tsstore .

# Run the container
docker run -d -v tsstore-data:/data -p 21080:21080 --name tsstore tsstore
```

Or use Docker Compose:

```bash
docker compose up -d
```

### Managing stores in Docker

The CLI commands run inside the container via `docker exec`:

```bash
# Create a new store
docker exec tsstore tsstore create my-store
# Output shows API key (save it!)

# List API keys for a store
docker exec tsstore tsstore key list my-store

# Regenerate API key (revokes existing keys)
docker exec tsstore tsstore key regenerate my-store
```

This design maintains security - key management requires container access, while all data operations use the REST API with authentication.

## Store Management

### Create a Store

```bash
# Create a store with defaults (1024 blocks, 4KB data/index, json type)
./tsstore create my-store

# Create with custom settings
./tsstore create sensors --blocks 10000 --data-size 8192

# Create a schema store for compact JSON
./tsstore create metrics --type schema

# Create in a specific directory
./tsstore create logs --path /var/tsstore
```

**Options:**
- `--blocks <n>` - Number of blocks (default: 1024)
- `--data-size <n>` - Data block size in bytes, must be power of 2 (default: 4096)
- `--index-size <n>` - Index block size in bytes, must be power of 2 (default: 4096)
- `--path <dir>` - Base directory for stores (default: ./data or TSSTORE_DATA_PATH)
- `--type <type>` - Data type: binary, text, json, schema (default: json)

## API Key Management

API keys can only be managed via CLI (requires device access):

```bash
# Regenerate key (revokes all existing keys)
./tsstore key regenerate my-store

# List keys (shows IDs only, not actual keys)
./tsstore key list my-store

# Revoke a specific key
./tsstore key revoke my-store a1b2c3d4
```

## Server Commands

### Start the Server

```bash
export TSSTORE_ADMIN_KEY="your-secure-admin-key-here"
./tsstore serve
```

**Options:**
- `--socket /path/to/socket.sock` - Unix socket path
- `--no-socket` - Disable Unix socket

### Swagger UI

```bash
./tsstore swagger
```

Starts a local file server on port 21090, serves `swagger.yaml` with CORS headers, and opens https://editor.swagger.io in your browser with the spec pre-loaded.

---

[Back to main README](README.md) | [API Reference](README-API.md) | [Data Input](README-DATA-INPUT.md) | [Data Output](README-DATA-OUTPUT.md)
