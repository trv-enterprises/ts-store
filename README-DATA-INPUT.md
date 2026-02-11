# ts-store Data Input Methods

[Back to main README](README.md)

This document covers all methods for writing data to a ts-store.

## REST API: Insert Data

```
POST /api/stores/:store/data
X-API-Key: <api-key>
Content-Type: application/json

{
  "timestamp": 1704067200000000000,
  "data": {"temperature": 72.5, "humidity": 45, "sensor": "living-room"}
}
```
Timestamp is optional (defaults to current time).

Returns:
```json
{
  "timestamp": 1704067200000000000,
  "block_num": 5,
  "size": 64
}
```

## WebSocket: Streaming Write

For high-frequency data ingestion over WebSocket.

```
GET /api/stores/:store/ws/write?api_key=<key>&format=full
```

**Query parameters:**
- `api_key` - Required for authentication
- `format` - For schema stores: `compact` or `full` (default: `full`)

**Client sends:**
```json
{"timestamp": 1234567890, "data": {...}}
```

**Server responds:**
```json
{"type": "ack", "timestamp": 1234567890, "block_num": 5, "size": 64}
{"type": "error", "message": "..."}
```

### Testing with websocat

```bash
# Install websocat
brew install websocat  # macOS

# Connect and send data
websocat "ws://localhost:21080/api/stores/my-store/ws/write?api_key=KEY"
# Then type: {"data": {"temp": 72.5}}

# For HTTPS/WSS
websocat -k "wss://localhost:21080/api/stores/my-store/ws/write?api_key=KEY"
```

## Unix Socket: Low-Latency Local Ingestion

For high-frequency local data ingestion with minimal overhead. Eliminates HTTP overhead and is ideal for sensor data collection on edge devices.

### Configuration

By default, the socket is created at `/var/run/tsstore/tsstore.sock`. Override with:
- Environment: `TSSTORE_SOCKET_PATH=/path/to/socket.sock`
- Config: `{"server": {"socket_path": "/path/to/socket.sock"}}`
- CLI: `tsstore serve --socket /path/to/socket.sock`
- Disable: `tsstore serve --no-socket`

### Protocol

1. Connect to the Unix socket
2. Send authentication: `AUTH <store-name> <api-key>\n`
3. Receive response: `OK\n` or `ERROR <message>\n`
4. Send JSON data lines: `{"field": "value"}\n`
5. Receive per-line response: `OK <timestamp>\n` or `ERROR <message>\n`
6. Send `QUIT\n` to disconnect

### Example (using netcat)

```bash
(
echo "AUTH my-store tsstore_xxxx-xxxx-xxxx"
echo '{"temp": 22.5, "humidity": 45.2}'
echo '{"temp": 22.6, "humidity": 45.1}'
echo "QUIT"
) | nc -U /var/run/tsstore/tsstore.sock
```

### Example (Python)

```python
import socket
import json

sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect('/var/run/tsstore/tsstore.sock')

# Authenticate
sock.send(b'AUTH my-store tsstore_xxxx-xxxx-xxxx\n')
response = sock.recv(1024)  # OK\n

# Send data
data = {"temp": 22.5, "humidity": 45.2}
sock.send((json.dumps(data) + '\n').encode())
response = sock.recv(1024)  # OK <timestamp>\n

sock.send(b'QUIT\n')
sock.close()
```

### Benefits over HTTP

- ~10x lower latency (microseconds vs milliseconds)
- No TCP/HTTP overhead
- Persistent connection for streaming data
- Ideal for high-frequency sensor sampling (100Hz+)

## Outbound Pull: Receive from Remote Server

ts-store can connect to a remote WebSocket server and receive data from it.

```
POST /api/stores/:store/ws/connections
X-API-Key: <api-key>
Content-Type: application/json

{
  "mode": "pull",
  "url": "wss://remote.example.com/data",
  "format": "full",
  "headers": {"Authorization": "Bearer token"}
}
```

**Connection parameters:**
- `mode` - `pull` (ts-store receives data from remote server)
- `url` - WebSocket URL to connect to
- `format` - `compact` or `full` for schema stores
- `headers` - Custom HTTP headers for connection

The connection automatically reconnects on failure and resumes from the last received timestamp.

---

[Back to main README](README.md) | [API Reference](README-API.md) | [Data Output](README-DATA-OUTPUT.md) | [CLI Reference](README-CLI.md)
