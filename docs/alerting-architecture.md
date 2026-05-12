# Alerting Architecture

This document describes the design of the ts-store alerting system as of v0.6.

## Overview

Alerting is a first-class, transport-independent feature. A **rule** is a condition over the fields of a stored record (e.g. `temperature > 80`). When a rule matches an incoming record, an **alert** is dispatched through a configured **sink**. Three sink types are supported, each as its own resource:

| Resource | Sink | Use case |
|---|---|---|
| `webhook_alerts` | HTTP POST to a URL | PagerDuty, Slack, custom hooks |
| `ws_alerts` | Open-on-fire outbound WebSocket frame | Existing WS consumers |
| `mqtt_alerts` | Publish to an MQTT topic (QoS 1) | Broker fan-out to many subscribers |

All three are persisted per-store as JSON, run as independent goroutines, and survive daemon restarts (re-loaded by the manager on boot).

> **Important:** Streaming data (`ws_connections`, `mqtt_connections`) and alerting (`*_alerts`) are now fully separate concerns. WS push and MQTT push deliver every record; alerts only fire on rule matches. Configure one or the other or both.

## Architecture

```
┌────────────────────────────────────────────────────────────────────────────┐
│ Store (per-store, owned by StoreService)                                   │
│                                                                            │
│  ┌──────────────────┐                                                      │
│  │ PutObject / data │  Records appended on the write path. Unchanged.      │
│  │ ingestion        │                                                      │
│  └────────┬─────────┘                                                      │
│           │                                                                │
│           ▼                                                                │
│   ┌───────────────┐                                                        │
│   │  Partition    │  Data is read back by range queries.                   │
│   │  store        │                                                        │
│   └───────┬───────┘                                                        │
│           │                                                                │
│           │  GetObjectsInRange(lastTs+1, now, 100)                         │
│           │  ▲                                                             │
└───────────┼──┼─────────────────────────────────────────────────────────────┘
            │  │
            │  │ poll every PollInterval (default 1s)
            │  │
┌───────────┼──┼─────────────────────────────────────────────────────────────┐
│ alerts.Manager (per-store)                                                 │
│           │  │                                                             │
│   ┌───────▼──┴────────┐                                                    │
│   │ alerts.Worker     │  One per configured alert resource.                │
│   │                   │  Range-polls the store, expands schema data,       │
│   │                   │  feeds each record into the Evaluator.             │
│   └───────┬───────────┘                                                    │
│           │                                                                │
│           │ Evaluate(ts, data)                                             │
│           ▼                                                                │
│   ┌───────────────────┐                                                    │
│   │ rules.Evaluator   │  Async worker. Rule match → cooldown check →       │
│   │ + cooldown map    │  invoke onAlert callback.                          │
│   └───────┬───────────┘                                                    │
│           │                                                                │
│           │ onAlert(alert)                                                 │
│           ▼                                                                │
│   ┌───────────────────┐                                                    │
│   │ Sink              │  Transport-specific dispatch:                      │
│   │  - WebhookSink    │   - HTTP POST (notify.Webhook async queue)         │
│   │  - WSSink         │   - Dial WS, write one frame, close                │
│   │  - MQTTSink       │   - Publish at QoS 1 on persistent client          │
│   └───────────────────┘                                                    │
└────────────────────────────────────────────────────────────────────────────┘
```

## Data flow

1. **Polling**: each Worker calls `store.GetObjectsInRange(lastTs+1, now, 100)` every `poll_interval` (default `1s`).
2. **Schema expansion**: for schema-type stores, records are expanded via `store.ExpandData` so condition fields resolve to schema field names.
3. **Evaluation**: parsed JSON is handed to `rules.Evaluator.Evaluate(ts, data)`, which buffers it in a 1000-element channel.
4. **Match → dispatch**: in a separate goroutine, each rule is checked; on a match, cooldown is enforced; on first-allowed match, the callback fires `Sink.Send(alert)`.
5. **Cursor**: the worker writes its `lastTs` to `<store>/<type>_alert_<id>.cursor` periodically. **On daemon restart, this file is not consulted**: workers start from `time.Now()` to avoid stale-alert stampedes on long outages. (See [Future work](#future-work).)

## Configuration

### File layout

Per store, the on-disk layout includes:

```
<store>/
  meta.tsdb
  partition-N/...
  ws_connections.json      ← streaming WS push (unchanged)
  mqtt_connections.json    ← streaming MQTT sink (unchanged)
  webhook_alerts.json      ← NEW
  ws_alerts.json           ← NEW
  mqtt_alerts.json         ← NEW
  *_alert_<id>.cursor      ← per-alert cursor file
```

### Common rule shape

All three alert resources share the same rule format:

```json
{
  "name": "high-temp",
  "condition": "temperature > 80",
  "cooldown": "5m"
}
```

`condition` syntax: `field <op> value`, optionally compounded with `AND` / `OR`. Operators: `==`, `!=`, `>`, `>=`, `<`, `<=`, `contains`. Values: numbers, quoted strings, booleans.

## HTTP API

All endpoints require the store's `X-API-Key` (same as streaming endpoints).

| Method | Path | Body | Description |
|---|---|---|---|
| POST | `/api/stores/{store}/alerts/webhook` | `CreateWebhookAlertRequest` | Create a webhook alert |
| POST | `/api/stores/{store}/alerts/ws` | `CreateWSAlertRequest` | Create a WS alert |
| POST | `/api/stores/{store}/alerts/mqtt` | `CreateMQTTAlertRequest` | Create an MQTT alert |
| GET  | `/api/stores/{store}/alerts` | — | List all alerts (all three types, tagged) |
| GET  | `/api/stores/{store}/alerts/{id}` | — | Get one alert's status |
| DELETE | `/api/stores/{store}/alerts/{id}` | — | Stop the worker and remove the persisted config |

### Webhook alert request

```json
{
  "url": "https://hooks.example.com/incoming",
  "headers": { "Authorization": "Bearer xyz", "X-Source": "ts-store" },
  "rules": [
    { "name": "high-temp", "condition": "temperature > 80", "cooldown": "5m" }
  ],
  "poll_interval": "1s",
  "timeout": "10s"
}
```

### WS alert request

```json
{
  "url": "wss://alerts.example.com/in",
  "headers": { "Authorization": "Bearer xyz" },
  "rules": [{ "name": "high-temp", "condition": "temperature > 80" }],
  "poll_interval": "1s"
}
```

Each alert opens a fresh outbound WS connection, sends one `{"type":"alert", "alert":{...}}` frame, and closes. No keep-alive connection.

### MQTT alert request

```json
{
  "broker_url": "tcp://broker.example.com:1883",
  "topic": "alerts/heat",
  "username": "u",
  "password": "p",
  "qos": 1,
  "rules": [{ "name": "high-temp", "condition": "temperature > 80" }],
  "poll_interval": "1s"
}
```

The MQTT client connects on first dispatch and stays connected. QoS defaults to 1 (at-least-once).

### Alert payload

The body POSTed to the webhook (and the contents of the `alert` field on a WS frame, and the MQTT publish body) is the same JSON shape across all three transports:

```json
{
  "rule_name": "high-temp",
  "condition": "temperature > 80",
  "timestamp": 1747000000000000000,
  "data": { "temperature": 95.0, "humidity": 0.4 },
  "store_name": "sensors"
}
```

`data` is the full record that triggered the match.

## CLI

```sh
# Webhook alert
tsstore alerts webhook add <store> --url <url> \
  --rule "high-temp:temperature > 80" --cooldown 5m \
  [--header Authorization:Bearer\ xyz] [--poll 1s] [--api-key $KEY]

# WS alert
tsstore alerts ws add <store> --url <ws-url> \
  --rule "high-temp:temperature > 80"

# MQTT alert
tsstore alerts mqtt add <store> --broker <url> --topic <t> \
  --rule "high-temp:temperature > 80" [--qos 1] [--username u --password p]

# List / delete
tsstore alerts list <store>
tsstore alerts rm   <store> <alert-id>
```

Multiple `--rule`/`--cooldown` pairs are supported; `--cooldown` applies to the *last* `--rule`.

## Status output

`tsstore status` lists each alert type next to the existing WS / MQTT connection summaries:

```
Store: sensors
  Type:           json
  Blocks:         5,485 / 6,144 (89.3% used)
  ...
  Webhook alerts: 1
    - a1b2c3d4: https://hooks.example.com/incoming (1 rules)
  MQTT alerts:    2
    - b2c3d4e5: tcp://mqtt:1883 -> alerts/temp (1 rules)
```

## Failure semantics

| Failure | Behavior |
|---|---|
| Webhook returns non-2xx | Logged; alert is *not* retried. |
| Webhook queue full (100) | Alert dropped, logged. Indicates downstream is too slow. |
| WS dial fails | Single-alert failure, logged. Worker continues; next match retries the dial. |
| MQTT publish fails | Worker logs and continues. The persistent client will auto-reconnect on its own. |
| Daemon crash mid-poll | Cursor file may be slightly stale; on restart, worker starts from "now" anyway. |
| Bad rule syntax in config | Worker construction fails at load time; logged; other alerts continue. |

## Future work

- **Optional replay from persisted cursor**: today's "start from now" default avoids stampedes after long outages, but legitimate alerts that fired during the outage are missed. A future `replay: "now" | "resume"` (or `replay_after: <duration>`) on each alert config would let users opt into resume semantics, possibly with a cap (e.g. "replay at most 1 hour"). See the corresponding follow-up issue.
- **Retry policies for webhook**: today, transient HTTP failures are not retried. A bounded exponential backoff inside `notify.Webhook` would be cheap to add.
- **TLS / mTLS for MQTT**: `tcp://` brokers only. `ssl://` (TLS) and `wss://` (MQTT-over-WebSocket) need config knobs.
- **Authentication on MQTT-over-WSS**: combine broker `wss://` URL with header auth for cloud-broker scenarios.
- **Per-alert metrics**: `alertsFired` is exposed in status but there's no histogram of latency, drop counts, or per-rule fire counts. Worth surfacing if alerts become widely used.
