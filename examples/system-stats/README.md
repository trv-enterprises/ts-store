# system-stats

A minimal-overhead system statistics collector that writes to ts-store.

## Features

- Reads directly from `/proc` (and `/sys/class/thermal` for temperature fallback) for minimal overhead
- Collects CPU, memory, swap, uptime, disk I/O, network I/O, disk space, and temperatures
- Supports Unix socket (low latency) or HTTP API
- Single static binary, no dependencies

## Building

```bash
# From ts-store root
cd examples/system-stats

# For Linux AMD64
GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o system-stats-linux-amd64 .

# For ARM64 (Raspberry Pi 4, etc.)
GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o system-stats-linux-arm64 .
```

## Usage

```bash
# Output to stdout (for testing)
./system-stats -stdout -interval 5

# Write to ts-store via Unix socket (recommended for local ts-store)
./system-stats -socket /var/run/tsstore/tsstore.sock \
               -store system-stats \
               -key tsstore_xxxx-xxxx-xxxx-xxxx \
               -interval 20

# Write to ts-store via HTTP (for remote or Docker deployments)
./system-stats -http http://localhost:21080 \
               -store system-stats \
               -key tsstore_xxxx-xxxx-xxxx-xxxx \
               -interval 20
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-socket` | `/var/run/tsstore/tsstore.sock` | ts-store Unix socket path |
| `-http` | (none) | ts-store HTTP URL (use instead of socket) |
| `-store` | `system-stats` | Store name |
| `-key` | (required) | API key (or set `TSSTORE_API_KEY` env var) |
| `-interval` | `20` | Collection interval in seconds |
| `-stdout` | `false` | Output to stdout instead of ts-store |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `TSSTORE_API_KEY` | API key (alternative to `-key` flag) |
| `TSSTORE_URL` | HTTP URL (alternative to `-http` flag) |

## Output Format

The collector outputs flat JSON with dot-notation field names (compatible with schema stores):

```json
{
  "cpu.pct": 15,
  "memory.total": 8388608000,
  "memory.used": 4194304000,
  "memory.available": 4194304000,
  "memory.pct": 50,
  "disk_io.read_bytes_sec": 1024000,
  "disk_io.write_bytes_sec": 512000,
  "network.rx_bytes_sec": 100000,
  "network.tx_bytes_sec": 50000,
  "disk_space.total": 100000000000,
  "disk_space.used": 50000000000,
  "disk_space.available": 50000000000,
  "disk_space.pct": 50,
  "swap.total": 2147483648,
  "swap.used": 0,
  "swap.pct": 0,
  "uptime.sec": 86400,
  "temp.cpu_package_c": 52.0,
  "temp.cpu_max_core_c": 58.0,
  "temp.nvme_c": 41.0
}
```

**Temperature fields** are tagged `omitempty` — hosts without a given sensor (no NVMe, no NIC temp, etc.) simply omit the field rather than report a misleading 0 °C. Supported fields:

| Field | Source | Notes |
|---|---|---|
| `temp.cpu_package_c` | `coretemp`/`k10temp` "Package" label | x86 desktops/servers |
| `temp.cpu_max_core_c` | hottest core under the same driver | x86 |
| `temp.cpu_c` | `/sys/class/thermal/thermal_zone*` (`cpu-thermal`/`cpu_thermal`) | ARM / Raspberry Pi fallback |
| `temp.nvme_c` | NVMe SMART | when present |
| `temp.pch_c` | platform controller hub | x86 |
| `temp.nic_c` | NIC hwmon | when present |

## Creating the Store

Create a schema-type store for compact storage (~50% smaller than JSON):

```bash
# Create the store
curl -X POST http://localhost:21080/api/stores \
  -H "X-Admin-Key: your-admin-key" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "system-stats",
    "num_blocks": 10000,
    "data_type": "schema"
  }'

# Save the returned API key!

# Set the schema
curl -X PUT http://localhost:21080/api/stores/system-stats/schema \
  -H "X-API-Key: your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "fields": [
      {"index": 1,  "name": "cpu.pct", "type": "int32"},
      {"index": 2,  "name": "memory.total", "type": "int64"},
      {"index": 3,  "name": "memory.used", "type": "int64"},
      {"index": 4,  "name": "memory.available", "type": "int64"},
      {"index": 5,  "name": "memory.pct", "type": "int32"},
      {"index": 6,  "name": "disk_io.read_bytes_sec", "type": "int64"},
      {"index": 7,  "name": "disk_io.write_bytes_sec", "type": "int64"},
      {"index": 8,  "name": "network.rx_bytes_sec", "type": "int64"},
      {"index": 9,  "name": "network.tx_bytes_sec", "type": "int64"},
      {"index": 10, "name": "disk_space.total", "type": "int64"},
      {"index": 11, "name": "disk_space.used", "type": "int64"},
      {"index": 12, "name": "disk_space.available", "type": "int64"},
      {"index": 13, "name": "disk_space.pct", "type": "int32"},
      {"index": 14, "name": "swap.total", "type": "int64"},
      {"index": 15, "name": "swap.used", "type": "int64"},
      {"index": 16, "name": "swap.pct", "type": "int32"},
      {"index": 17, "name": "uptime.sec", "type": "int64"},
      {"index": 18, "name": "temp.cpu_package_c", "type": "float64"},
      {"index": 19, "name": "temp.cpu_max_core_c", "type": "float64"},
      {"index": 20, "name": "temp.cpu_c", "type": "float64"},
      {"index": 21, "name": "temp.nvme_c", "type": "float64"},
      {"index": 22, "name": "temp.pch_c", "type": "float64"},
      {"index": 23, "name": "temp.nic_c", "type": "float64"}
    ]
  }'
```

Alternatively, create a JSON-type store (larger but no schema needed):

```bash
curl -X POST http://localhost:21080/api/stores \
  -H "X-Admin-Key: your-admin-key" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "system-stats",
    "num_blocks": 10000,
    "data_type": "json"
  }'
```

## Systemd Service

Example service file (`/etc/systemd/system/system-stats.service`):

```ini
[Unit]
Description=System Stats Collector for ts-store
After=tsstore.service
Requires=tsstore.service

[Service]
Type=simple
User=youruser
ExecStartPre=/bin/sleep 5
ExecStart=/home/youruser/bin/system-stats -socket /var/run/tsstore/tsstore.sock -store system-stats -interval 20
Restart=always
RestartSec=10
Environment=TSSTORE_API_KEY=tsstore_xxxx-xxxx-xxxx-xxxx

[Install]
WantedBy=multi-user.target
```

Install and enable:

```bash
sudo cp system-stats.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable system-stats
sudo systemctl start system-stats
```

## Storage Calculations

With default settings (20-second interval, ~320 bytes per reading):
- 10,000 blocks × 4KB = 40MB storage
- ~12 readings per block = ~120,000 readings
- At 20-second intervals = ~28 days of data

Adjust `num_blocks` when creating the store based on your retention needs.
