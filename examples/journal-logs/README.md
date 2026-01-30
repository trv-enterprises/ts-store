# journal-logs

Streams journalctl output to ts-store via Unix socket or HTTP.

## Features

- Streams journalctl in real-time using `journalctl -f`
- Parses JSON output for structured log entries
- Supports Unix socket (low latency) or HTTP API
- Optional filtering by unit, priority, or time
- Automatic reconnection on socket errors
- Single static binary, no dependencies

## Building

```bash
# From ts-store root
cd examples/journal-logs

# For Linux AMD64
GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o journal-logs-linux-amd64 .

# For ARM64 (Raspberry Pi 4, etc.)
GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o journal-logs-linux-arm64 .
```

## Usage

```bash
# Output to stdout (for testing)
./journal-logs -stdout

# Write to ts-store via Unix socket (recommended)
./journal-logs -socket /var/run/tsstore/tsstore.sock \
               -store journal-logs \
               -key tsstore_xxxx-xxxx-xxxx-xxxx

# Write to ts-store via HTTP
./journal-logs -http http://localhost:21080 \
               -store journal-logs \
               -key tsstore_xxxx-xxxx-xxxx-xxxx

# Filter by units
./journal-logs -socket /var/run/tsstore/tsstore.sock \
               -store journal-logs \
               -key tsstore_xxxx \
               -units "sshd,nginx,docker"

# Filter by priority (0=emerg to 7=debug)
./journal-logs -priority 4  # warning and above

# Start from specific time
./journal-logs -since "1 hour ago"
./journal-logs -since "today"
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-socket` | `/var/run/tsstore/tsstore.sock` | ts-store Unix socket path |
| `-http` | (none) | ts-store HTTP URL (use instead of socket) |
| `-store` | `journal-logs` | Store name |
| `-key` | (required) | API key (or set `TSSTORE_API_KEY` env var) |
| `-stdout` | `false` | Output to stdout instead of ts-store |
| `-since` | (none) | Start from this time (e.g., "1 hour ago") |
| `-units` | (none) | Comma-separated units to filter |
| `-priority` | (none) | Max priority level (0-7) |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `TSSTORE_API_KEY` | API key (alternative to `-key` flag) |
| `TSSTORE_URL` | HTTP URL (alternative to `-http` flag) |

## Output Format

Each journal entry is stored as JSON:

```json
{
  "time": "2026-01-29T15:30:45Z",
  "host": "trv-srv-001",
  "unit": "sshd.service",
  "ident": "sshd",
  "msg": "Accepted publickey for user from 192.168.1.100",
  "pri": 6,
  "pid": 12345
}
```

Fields:
- `time` - RFC3339 timestamp
- `host` - Hostname
- `unit` - Systemd unit (if applicable)
- `ident` - Syslog identifier or command name
- `msg` - Log message
- `pri` - Priority (0=emerg, 1=alert, 2=crit, 3=err, 4=warning, 5=notice, 6=info, 7=debug)
- `pid` - Process ID

## Creating the Store

Create a JSON-type store (journal entries are variable/unstructured):

```bash
curl -X POST http://localhost:21080/api/stores \
  -H "X-Admin-Key: your-admin-key" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "journal-logs",
    "num_blocks": 50000,
    "data_type": "json"
  }'

# Save the returned API key!
```

## Systemd Service

Example service file (`/etc/systemd/system/journal-logs.service`):

```ini
[Unit]
Description=Journal Log Collector for ts-store
After=tsstore.service
Requires=tsstore.service

[Service]
Type=simple
User=root
ExecStartPre=/bin/sleep 5
ExecStart=/home/tviviano/bin/journal-logs -socket /var/run/tsstore/tsstore.sock -store journal-logs
Restart=always
RestartSec=10
Environment=TSSTORE_API_KEY=tsstore_xxxx-xxxx-xxxx-xxxx

[Install]
WantedBy=multi-user.target
```

**Note:** The service runs as root to access all journal entries. Use `-units` to filter specific services if you want to run as a regular user.

Install and enable:

```bash
sudo cp journal-logs.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable journal-logs
sudo systemctl start journal-logs
```

## Storage Calculations

Journal log volume varies widely. Rough estimates:
- Quiet server: ~100-500 entries/hour
- Active server: ~1000-5000 entries/hour
- Busy server: ~10000+ entries/hour

With 50,000 blocks (4KB each = 200MB):
- ~30-50 entries per block (depends on message length)
- 1.5-2.5 million log entries total
- At 1000 entries/hour = ~60-100 days retention

Adjust `num_blocks` based on your log volume and retention needs.

## Querying Logs

```bash
# Get recent logs
curl -s -H "X-API-Key: <key>" \
  "http://localhost:21080/api/stores/journal-logs/data/newest?limit=50" | jq .

# Search for errors
curl -s -H "X-API-Key: <key>" \
  "http://localhost:21080/api/stores/journal-logs/data/newest?limit=100&filter=error" | jq .

# Get logs from time range
curl -s -H "X-API-Key: <key>" \
  "http://localhost:21080/api/stores/journal-logs/data/range?since=1h" | jq .
```
