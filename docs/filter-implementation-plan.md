<!--
Copyright (c) 2026 TRV Enterprises LLC
Licensed under the Business Source License 1.1
See LICENSE file for details.
-->

# Filter Implementation Plan

## Overview

Add simple substring filtering to data retrieval operations across all three integration points:

1. **REST API queries** - Filter results via query parameters
2. **Inbound WebSocket read streams** - Filter streamed data via connection parameters
3. **Outbound push connections** - Filter pushed data via connection configuration

## Design Principles

- **Simple substring matching** using `bytes.Contains` (zero binary impact)
- **Case sensitivity** controlled by optional parameter (default: case-sensitive)
- **Post-retrieval filtering** - filter after reading data from store
- **Consistent API** - same parameter names across all integration points

## Filter Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `filter` | string | "" (no filter) | Substring to match in object data |
| `filter_ignore_case` | bool | false | If true, match case-insensitively |

## Implementation Details

### 1. Core Filter Function

New file: `pkg/store/filter.go`

```go
package store

import "bytes"

// MatchesFilter returns true if data contains the filter pattern.
// If filter is empty, always returns true.
func MatchesFilter(data []byte, filter string, ignoreCase bool) bool {
    if filter == "" {
        return true
    }
    if ignoreCase {
        return bytes.Contains(bytes.ToLower(data), bytes.ToLower([]byte(filter)))
    }
    return bytes.Contains(data, []byte(filter))
}
```

### 2. REST API Changes

**Affected endpoints:**
- `GET /api/stores/:store/data/oldest`
- `GET /api/stores/:store/data/newest`
- `GET /api/stores/:store/data/range`

**New query parameters:**
- `filter` - substring to match
- `filter_ignore_case` - "true" for case-insensitive matching

**File changes:** `internal/handlers/unified_handler.go`

For each endpoint, after retrieving handles, filter during data fetch:

```go
// In ListRange, ListOldest, ListNewest:
filterStr := c.Query("filter")
filterIgnoreCase := c.Query("filter_ignore_case") == "true"

// When building response:
for _, handle := range handles {
    data, err := st.GetObject(handle)
    if err != nil {
        continue
    }

    // Apply filter
    if !store.MatchesFilter(data, filterStr, filterIgnoreCase) {
        continue
    }

    // Add to response...
}
```

**Note:** When filtering is active with `include_data=false`, we still need to fetch data to filter, but we don't include it in the response.

### 3. Inbound WebSocket Read Changes

**Endpoint:** `GET /api/stores/:store/ws/read`

**New query parameters:**
- `filter` - substring to match
- `filter_ignore_case` - "true" for case-insensitive matching

**File changes:** `internal/handlers/ws_reader.go`

Add filter fields to `wsReader` struct:

```go
type wsReader struct {
    conn             *websocket.Conn
    store            *store.Store
    from             int64
    format           string
    filter           string  // NEW
    filterIgnoreCase bool    // NEW
    closeCh          chan struct{}
    lastSent         int64
    caughtUp         bool
}
```

Update `newWSReader` to parse filter params:

```go
func newWSReader(conn *websocket.Conn, st *store.Store, fromStr, format, filter string, filterIgnoreCase bool) (*wsReader, error) {
    // ... existing code ...
    return &wsReader{
        // ... existing fields ...
        filter:           filter,
        filterIgnoreCase: filterIgnoreCase,
    }, nil
}
```

Update `sendData` to filter before sending:

```go
func (r *wsReader) sendData(handle *store.ObjectHandle, data []byte) error {
    // Apply filter
    if !store.MatchesFilter(data, r.filter, r.filterIgnoreCase) {
        return nil // Skip this object
    }

    // ... existing send logic ...
}
```

**File changes:** `internal/handlers/ws_handler.go`

Update `Read` handler to pass filter params:

```go
func (h *WSHandler) Read(c *gin.Context) {
    // ... existing code ...
    filter := c.Query("filter")
    filterIgnoreCase := c.Query("filter_ignore_case") == "true"

    reader, err := newWSReader(conn, st, from, format, filter, filterIgnoreCase)
    // ...
}
```

### 4. Outbound Push Connection Changes

**Endpoint:** `POST /api/stores/:store/ws/connections`

**New request fields:**
```json
{
    "mode": "push",
    "url": "wss://...",
    "filter": "sensor:01",
    "filter_ignore_case": false
}
```

**File changes:**

#### `pkg/store/ws_config.go`

Add filter fields to `WSConnection`:

```go
type WSConnection struct {
    ID               string            `json:"id"`
    Mode             string            `json:"mode"`
    URL              string            `json:"url"`
    From             int64             `json:"from"`
    Format           string            `json:"format"`
    Headers          map[string]string `json:"headers"`
    Filter           string            `json:"filter,omitempty"`            // NEW
    FilterIgnoreCase bool              `json:"filter_ignore_case,omitempty"` // NEW
    CreatedAt        time.Time         `json:"created_at"`
}
```

#### `internal/handlers/ws_connections.go`

Update `CreateRequest`:

```go
type CreateRequest struct {
    Mode             string            `json:"mode" binding:"required"`
    URL              string            `json:"url" binding:"required"`
    From             int64             `json:"from,omitempty"`
    Format           string            `json:"format,omitempty"`
    Headers          map[string]string `json:"headers,omitempty"`
    Filter           string            `json:"filter,omitempty"`             // NEW
    FilterIgnoreCase bool              `json:"filter_ignore_case,omitempty"` // NEW
}
```

Update `Create` handler to pass filter fields.

#### `internal/ws/manager.go`

Update `CreateConnectionRequest`:

```go
type CreateConnectionRequest struct {
    Mode             string            `json:"mode"`
    URL              string            `json:"url"`
    From             int64             `json:"from"`
    Format           string            `json:"format"`
    Headers          map[string]string `json:"headers"`
    Filter           string            `json:"filter"`             // NEW
    FilterIgnoreCase bool              `json:"filter_ignore_case"` // NEW
}
```

Update `CreateConnection` to include filter in `WSConnection`.

#### `internal/ws/pusher.go`

Update `sendNewData` to filter before sending:

```go
func (p *Pusher) sendNewData() error {
    // ... existing code to get handles ...

    for _, handle := range handles {
        data, err := p.store.GetObject(handle)
        if err != nil {
            continue
        }

        // Apply filter
        if !store.MatchesFilter(data, p.config.Filter, p.config.FilterIgnoreCase) {
            continue
        }

        // ... existing send logic ...
    }
}
```

#### `internal/ws/status.go` (or within existing types)

Update `ConnectionStatus` to show filter configuration:

```go
type ConnectionStatus struct {
    // ... existing fields ...
    Filter           string `json:"filter,omitempty"`
    FilterIgnoreCase bool   `json:"filter_ignore_case,omitempty"`
}
```

## API Examples

### REST API

```bash
# Get newest objects containing "temperature"
curl "http://localhost:8080/api/stores/sensors/data/newest?limit=10&filter=temperature&include_data=true"

# Get range with case-insensitive filter
curl "http://localhost:8080/api/stores/sensors/data/range?since=1h&filter=BUILDING+A&filter_ignore_case=true&include_data=true"
```

### Inbound WebSocket Read

```javascript
// Connect with filter
const ws = new WebSocket('ws://localhost:8080/api/stores/sensors/ws/read?api_key=KEY&from=0&filter=sensor:01');

ws.onmessage = (event) => {
    const msg = JSON.parse(event.data);
    // Only receives objects containing "sensor:01"
};
```

### Outbound Push Connection

```bash
# Create filtered push connection
curl -X POST http://localhost:8080/api/stores/sensors/ws/connections \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer API_KEY" \
  -d '{
    "mode": "push",
    "url": "wss://remote.example.com/data",
    "from": 0,
    "filter": "building:north",
    "filter_ignore_case": true
  }'
```

## Files to Change

| File | Changes |
|------|---------|
| `pkg/store/filter.go` | NEW - Core filter function |
| `internal/handlers/unified_handler.go` | Add filter params to list endpoints |
| `internal/handlers/ws_handler.go` | Pass filter params to reader |
| `internal/handlers/ws_reader.go` | Add filter fields, filter in sendData |
| `internal/handlers/ws_connections.go` | Add filter to CreateRequest |
| `pkg/store/ws_config.go` | Add filter fields to WSConnection |
| `internal/ws/manager.go` | Add filter to CreateConnectionRequest |
| `internal/ws/pusher.go` | Filter in sendNewData |
| `swagger.yaml` | Document new parameters |
| `README.md` | Document filter feature |

## Testing

1. **Unit tests** for `MatchesFilter` function
2. **REST API tests** - verify filtered results
3. **WebSocket inbound tests** - verify filtered stream
4. **WebSocket outbound tests** - verify filtered push

## Binary Size Impact

- Using `bytes.Contains` and `bytes.ToLower` from standard library
- Estimated additional binary size: **~0 KB** (already imported)

## Future Enhancements (Not in Scope)

- Multiple filters (AND logic)
- Regex support (opt-in, +1.5-2MB binary)
- JSON path filtering (filter on specific fields)
- Negative filters (exclude matching objects)
