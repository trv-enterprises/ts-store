# Alerting Architecture

This document describes the design and architecture of the tsstore alerting system.

## Overview

The alerting system enables rule-based notifications on data streams. When data matches configured conditions, alerts are fired via WebSocket messages and/or HTTP webhooks. The system is designed for high-throughput scenarios where rule evaluation must not block the primary data path.

## Design Goals

1. **Non-blocking** - Rule evaluation and webhook calls must not slow down data ingestion or streaming
2. **Lock-free evaluation** - Rules are evaluated outside the store's lock window
3. **Backwards compatible** - Existing connections work unchanged; alerting is opt-in
4. **Efficient** - Minimal overhead when no rules are configured
5. **Reliable** - Alerts are buffered and retried; slow webhooks don't cause data loss

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│ PUSHER (per WebSocket connection)                                                   │
│                                                                                     │
│  ┌──────────────┐     ┌─────────────────┐     ┌──────────────────────────────────┐ │
│  │              │     │                 │     │                                  │ │
│  │  Store Read  │────>│  Data Processing│────>│  WebSocket Send                  │ │
│  │  (locked)    │     │  (filter, agg)  │     │  (data messages)                 │ │
│  │              │     │                 │     │                                  │ │
│  └──────────────┘     └────────┬────────┘     └──────────────────────────────────┘ │
│                                │                                                    │
│                                │ Evaluate() - non-blocking                          │
│                                ▼                                                    │
│                       ┌─────────────────┐                                          │
│                       │                 │                                          │
│                       │  Channel (1000) │  Buffered queue decouples                │
│                       │                 │  data path from evaluation               │
│                       └────────┬────────┘                                          │
│                                │                                                    │
│                                ▼                                                    │
│  ┌─────────────────────────────────────────────────────────────────────────────┐   │
│  │ EVALUATOR GOROUTINE (started only if rules configured)                      │   │
│  │                                                                             │   │
│  │  ┌───────────────┐     ┌───────────────┐     ┌────────────────────────────┐ │   │
│  │  │               │     │               │     │                            │ │   │
│  │  │  Rule Match   │────>│  Cooldown     │────>│  Alert Dispatch            │ │   │
│  │  │  Evaluation   │     │  Check        │     │                            │ │   │
│  │  │               │     │               │     │  ┌──────────────────────┐  │ │   │
│  │  └───────────────┘     └───────────────┘     │  │ WebSocket callback   │  │ │   │
│  │                                              │  │ (type: "alert")      │  │ │   │
│  │                                              │  └──────────────────────┘  │ │   │
│  │                                              │                            │ │   │
│  │                                              │  ┌──────────────────────┐  │ │   │
│  │                                              │  │ Webhook Queue        │  │ │   │
│  │                                              │  │ (async HTTP POST)    │  │ │   │
│  │                                              │  └──────────────────────┘  │ │   │
│  │                                              └────────────────────────────┘ │   │
│  └─────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

## Components

### Rules Engine (`internal/rules/rules.go`)

Parses and evaluates condition expressions against data records.

**Condition Syntax:**
```
condition     = simple_cond | compound_cond
simple_cond   = field operator value
compound_cond = simple_cond ("AND" | "OR") simple_cond [("AND" | "OR") simple_cond ...]
operator      = ">" | ">=" | "<" | "<=" | "==" | "!="
value         = number | quoted_string | boolean
```

**Examples:**
- `temperature > 80`
- `status == "error"`
- `temperature > 80 AND humidity < 30`
- `level == "warn" OR level == "error"`

**Implementation:**
- Regex-based parser for simple conditions
- Type coercion: numeric comparison when both values are numbers, string comparison otherwise
- Short-circuit evaluation for AND/OR

### Evaluator (`internal/rules/evaluator.go`)

Manages async rule evaluation with cooldown tracking.

**Responsibilities:**
- Receives data records via channel (non-blocking)
- Evaluates all configured rules against each record
- Tracks last-fired time per rule for cooldown enforcement
- Dispatches alerts to callback and webhooks

**Concurrency Model:**
- Single goroutine per Pusher (only started if rules configured)
- Buffered channel (1000 records) absorbs bursts
- If channel is full, records are dropped (prevents backpressure)

### Webhook Notifier (`internal/notify/webhook.go`)

Handles async HTTP webhook delivery.

**Features:**
- Buffered queue (100 alerts per webhook)
- Configurable timeout (default 10s)
- Fire-and-forget with logging on failure
- Graceful shutdown with drain timeout

**Payload Format:**
```json
{
  "rule_name": "high_temp",
  "condition": "temperature > 80",
  "timestamp": 1707012345678901234,
  "data": {"temperature": 85.5, "humidity": 45.2},
  "store_name": "sensor-data"
}
```

## Data Flow

### Without Rules (existing behavior, unchanged)

```
Store.GetObjectsInRange() → filter → [aggregate] → WebSocket.WriteJSON()
        (locked)                                      (data message)
```

### With Rules

```
Store.GetObjectsInRange() → filter → [aggregate] → WebSocket.WriteJSON()
        (locked)               │                      (data message)
                               │
                               └──> Evaluator.Evaluate()  ← non-blocking channel send
                                           │
                                    (async goroutine)
                                           │
                                    Rule matches? → Cooldown OK? → Dispatch alert
                                                                        │
                                                         ┌──────────────┴──────────────┐
                                                         ▼                             ▼
                                                  WebSocket.WriteJSON()         Webhook.Send()
                                                    (alert message)            (async HTTP POST)
```

## Configuration

### WSConnection Extension

```go
type WSConnection struct {
    // ... existing fields ...
    Rules []AlertRuleConfig `json:"rules,omitempty"`
}

type AlertRuleConfig struct {
    Name           string            `json:"name"`
    Condition      string            `json:"condition"`
    Webhook        string            `json:"webhook,omitempty"`
    WebhookHeaders map[string]string `json:"webhook_headers,omitempty"`
    Cooldown       string            `json:"cooldown,omitempty"`
}
```

### Example Configuration

```json
{
  "mode": "push",
  "url": "ws://dashboard:8080/metrics",
  "rules": [
    {
      "name": "high_temp",
      "condition": "temperature > 80",
      "webhook": "https://pagerduty.com/alerts",
      "webhook_headers": {"Authorization": "Bearer xxx"},
      "cooldown": "5m"
    },
    {
      "name": "low_battery",
      "condition": "battery < 10 AND charging == false",
      "cooldown": "1h"
    }
  ]
}
```

## Performance Considerations

### Channel Sizing

The evaluator uses a buffered channel of 1000 records. This provides:
- **Burst absorption**: High-frequency data won't block the sender
- **Memory bound**: ~1000 * record_size bytes maximum queue
- **Drop policy**: If full, new records are dropped (logged)

For most use cases, 1000 is sufficient. If you're processing >10,000 records/second with slow rules, consider:
- Simpler rule conditions
- Aggregation to reduce record volume before alerting

### Webhook Queue

Each webhook has a 100-alert buffer. If a webhook endpoint is slow:
- Alerts queue up to 100
- Beyond 100, alerts are dropped (logged)
- Webhook timeout is 10 seconds

### Cooldown

Cooldown prevents alert storms. Without cooldown, a condition like `temperature > 80` would fire on every record while temperature is high. With `cooldown: "5m"`, only one alert fires per 5-minute window.

**Implementation:**
- `map[string]time.Time` tracks last-fired time per rule name
- Check is O(1) with mutex protection
- Cooldown of 0 means no limit (alert on every match)

## Thread Safety

| Component | Concurrency Model |
|-----------|------------------|
| Pusher | Single goroutine + mutex for conn/status |
| Evaluator | Single goroutine, receives via channel |
| Webhook | Single goroutine per webhook instance |
| Cooldown map | Protected by Evaluator's mutex |

The key design principle: **no locks are held across the Evaluate() call**. The channel send is non-blocking, so the data path proceeds immediately.

## Error Handling

| Error | Behavior |
|-------|----------|
| Invalid rule condition | Logged at startup, rule skipped |
| Channel full | Record dropped, no log (too noisy) |
| Webhook failure | Logged, alert dropped |
| WebSocket send failure | Logged, counter incremented |

## Monitoring

Connection status includes alerting metrics:

```json
{
  "id": "abc123",
  "status": "connected",
  "rules_count": 2,
  "alerts_fired": 47,
  "messages_sent": 15234,
  "errors": 0
}
```

## Future Enhancements

Potential improvements for future versions:

1. **Alert acknowledgment** - Track alert state (firing/resolved)
2. **Hysteresis** - Require condition to be true for N seconds before firing
3. **Rate limiting** - Global alert rate limit across all rules
4. **Alert grouping** - Batch multiple alerts into single webhook call
5. **Dead letter queue** - Persist failed webhook calls for retry
6. **Rule templates** - Predefined rules for common conditions
