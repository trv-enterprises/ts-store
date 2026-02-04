# trv-pi-001 Deployment

Deployment artifacts for tsstore on trv-pi-001 (Raspberry Pi).

## Directory Layout on Pi

```
~/bin/tsstore                              # binary
~/data/                                    # tsstore data directory
~/run/tsstore.sock                         # unix socket
~/tsstore/
├── scripts/
│   ├── journal-to-tsstore.sh              # journal log streamer (REST)
│   ├── sensehat-to-tsstore.py             # SenseHat collector (socket)
│   └── system-stats-to-tsstore.py         # system stats collector (socket)
├── services/
│   ├── tsstore.service
│   ├── journal-to-tsstore.service
│   ├── sensehat-to-tsstore.service
│   └── system-stats-to-tsstore.service
└── deploy.sh
```

## Stores

| Store | API Key | Protocol | Interval |
|-------|---------|----------|----------|
| journal-logs | `tsstore_467d0c08-...` | REST | streaming |
| sensehat | `tsstore_8bd1bfd5-...` | Unix socket | 1 Hz |
| system-stats | `tsstore_df14064e-...` | Unix socket | 10s |

## Deploy

```bash
# From this directory on the Pi:
./scripts/deploy.sh v0.3.0-rc1
```

Or manually:

```bash
# Build on dev machine
GOOS=linux GOARCH=arm64 go build -o tsstore-linux-arm64 ./cmd/tsstore

# Copy to Pi
scp tsstore-linux-arm64 tviviano@192.168.1.34:~/bin/tsstore

# Restart
ssh tviviano@192.168.1.34 'systemctl --user restart tsstore'
```

## Services

All services run as user-level systemd units:

```bash
systemctl --user status tsstore
systemctl --user status journal-to-tsstore
systemctl --user status sensehat-to-tsstore
systemctl --user status system-stats-to-tsstore
```
