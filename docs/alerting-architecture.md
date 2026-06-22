# Alerting Architecture

This document describes the design of the ts-store alerting system as of v0.9.0. The alerting design is unchanged since v0.6.8 — each alert still runs its own polling worker (see [Architecture](#architecture)). A future refactor to reduce the per-alert polling cost is tracked but not yet scheduled (see [Future work](#future-work)).

## Overview

Alerting is a first-class, transport-independent feature. Each **alert** carries exactly one **rule** — a condition over the fields of a stored record (e.g. `temperature > 80`) — and a **sink** that dispatches a payload when the rule matches. Three sink types are supported, each as its own on-disk resource:

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
5. **Cursor & restart policy**: each alert configures its own `restart_policy` (default `"now"`):
   - `"now"` — the worker starts from `time.Now()` on every Start and skips cursor writes entirely. Best for high-frequency metric streams where a brief gap is acceptable.
   - `"resume"` — the worker reads `<store>/<type>_alert_<id>.cursor` on Start and replays records since `lastTs`. If `max_replay` is set, the resume window is bounded to `now - max_replay` to cap the work after long outages. Best for event streams (e.g. journal logs) where a missed match is meaningful.

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

### Common alert fields

Every alert — webhook, WS, MQTT — carries the same rule + dispatch policy fields (the `AlertCommon` block, embedded into each transport-specific config):

| Field | Required | Description |
|---|---|---|
| `name` | yes | Human label for this alert. |
| `condition` | yes | Rule expression, e.g. `temperature > 80`. |
| `cooldown` | no | Min time between fires (duration string, e.g. `5m`). |
| `external_ref` | no | Opaque pass-through string (≤512 bytes, no NUL); echoed verbatim on every alert payload. |
| `restart_policy` | no | `"now"` (default — start from wall-clock now, no cursor) or `"resume"` (replay from cursor on restart). |
| `max_replay` | no | Duration cap on resume replay window. Only valid when `restart_policy="resume"`. Default: unbounded. |
| `poll_interval` | no | How often the worker polls the store (default `1s`). |

`condition` syntax: `field <op> value`, optionally compounded with `AND` / `OR`. Operators: `==`, `!=`, `>`, `>=`, `<`, `<=`, `contains`. Values: numbers, quoted strings, booleans. Field names may include dots and hyphens (`cpu.percent`, `temp.cpu_c`).

## HTTP API

All endpoints require the store's `X-API-Key` (same as streaming endpoints).

| Method | Path | Body | Description |
|---|---|---|---|
| POST   | `/api/stores/{store}/alerts` | `CreateAlertRequest` | Create an alert. The `type` field discriminates webhook / ws / mqtt. |
| GET    | `/api/stores/{store}/alerts` | — | List all alerts (all three types, tagged with `type`). |
| GET    | `/api/stores/{store}/alerts/{id}` | — | Get one alert's runtime status + persisted config (secrets redacted). |
| DELETE | `/api/stores/{store}/alerts/{id}` | — | Stop the worker and remove the persisted config. |

### Create request

The POST body has a `type` discriminator (`"webhook"` | `"ws"` | `"mqtt"`) and exactly one matching nested options object. The common alert fields (above) live at the top level.

#### Webhook

```json
{
  "type": "webhook",
  "name": "high-temp",
  "condition": "temperature > 80",
  "cooldown": "5m",
  "poll_interval": "1s",
  "webhook": {
    "url": "https://hooks.example.com/incoming",
    "headers": { "Authorization": "Bearer xyz", "X-Source": "ts-store" },
    "timeout": "10s"
  }
}
```

#### WebSocket

```json
{
  "type": "ws",
  "name": "high-temp",
  "condition": "temperature > 80",
  "ws": {
    "url": "wss://alerts.example.com/in",
    "headers": { "Authorization": "Bearer xyz" }
  }
}
```

Each fire opens a fresh outbound WS connection, sends one `{"type":"alert", "alert":{...}}` frame, and closes. No keep-alive connection.

#### MQTT

```json
{
  "type": "mqtt",
  "name": "high-temp",
  "condition": "temperature > 80",
  "mqtt": {
    "broker_url": "tcp://broker.example.com:1883",
    "topic": "alerts/heat",
    "username": "u",
    "password": "p",
    "qos": 1
  }
}
```

The MQTT client connects on first dispatch and stays connected. `qos` defaults to 1 (at-least-once) when 0 or omitted.

### Validation

The server rejects requests where `type` is missing or unknown, where the nested options block for the chosen `type` is absent, or where any of the *other* transport blocks are also set. `max_replay` without `restart_policy="resume"` is rejected. `external_ref` over 512 bytes or containing NUL bytes is rejected.

### Alert payload

The body POSTed to the webhook (and the contents of the `alert` field on a WS frame, and the MQTT publish body) is the same JSON shape across all three transports:

```json
{
  "rule_name": "high-temp",
  "condition": "temperature > 80",
  "timestamp": 1747000000000000000,
  "data": { "temperature": 95.0, "humidity": 0.4 },
  "store_name": "sensors",
  "external_ref": "dashboards/warehouse-sensors#component-42"
}
```

`data` is the full record that triggered the match. `external_ref` is omitted when the rule didn't configure one.

#### `external_ref` — opaque pass-through

Each alert may carry an `external_ref` string (≤512 bytes, no NUL bytes, otherwise unconstrained). ts-store does not parse or interpret it — receivers can stash whatever they need: a dashboard component id, a Grafana slug, a Slack channel name, or a JSON-encoded compound key like `{"dashboard_id":"…","namespace":"default"}`. When the alert fires, the value is echoed verbatim on the alert payload above. If the alert didn't set one, the field is omitted from the JSON.

## CLI

The CLI mirrors the HTTP API. Each transport has its own `add` subcommand, but all share the same rule + dispatch flags (`--name`, `--condition`, `--cooldown`, `--external-ref`, `--restart`, `--max-replay`, `--poll`, `--api-key`).

```sh
# Webhook alert
tsstore alerts webhook add <store> --url <url> \
  --name high-temp --condition "temperature > 80" --cooldown 5m \
  [--header Authorization:Bearer\ xyz] [--timeout 10s] \
  [--restart now|resume] [--max-replay 1h] [--external-ref <s>] \
  [--poll 1s] [--api-key $KEY]

# WS alert
tsstore alerts ws add <store> --url <ws-url> \
  --name high-temp --condition "temperature > 80" \
  [--header K:V] [--restart now|resume] [--max-replay 1h]

# MQTT alert
tsstore alerts mqtt add <store> --broker <url> --topic <t> \
  --name high-temp --condition "temperature > 80" \
  [--qos 0|1|2] [--username u --password p] \
  [--restart now|resume] [--max-replay 1h]

# List / delete
tsstore alerts list <store>
tsstore alerts rm   <store> <alert-id>
```

Each invocation creates one alert with one rule. To attach multiple rules to the same target, create multiple alerts.

## Status output

`tsstore status` lists each alert type next to the existing WS / MQTT connection summaries:

```
Store: sensors
  Type:           json
  Blocks:         5,485 / 6,144 (89.3% used)
  ...
  Webhook alerts: 1
    - a1b2c3d4: high-temp -> https://hooks.example.com/incoming
  MQTT alerts:    2
    - b2c3d4e5: high-temp -> tcp://mqtt:1883 alerts/temp
```

## Failure semantics

| Failure | Behavior |
|---|---|
| Webhook returns non-2xx | Logged; alert is *not* retried. |
| Webhook queue full (100) | Alert dropped, logged. Indicates downstream is too slow. |
| WS dial fails | Single-alert failure, logged. Worker continues; next match retries the dial. |
| MQTT publish fails | Worker logs and continues. The persistent client will auto-reconnect on its own. |
| Daemon crash mid-poll | If `restart_policy="now"` (default), the worker starts from "now" on restart and any in-flight matches are lost. If `restart_policy="resume"`, the worker replays from the last persisted cursor (optionally bounded by `max_replay`). |
| Bad rule syntax in config | Worker construction fails at load time; logged; other alerts continue. |

## Future work

- **Reduce N×scan cost of per-alert polling** ([#4](https://github.com/trv-enterprises/ts-store/issues/4)): each alert runs its own poll loop, so N alerts on one store do N redundant `GetObjectsInRange` scans per second. This is fine at the current scale (1–3 alerts per store) but grows linearly with alert count — relevant because the dashboard's rule wizard creates one alert per rule. Issue #4 is the canonical tracking item and weighs two candidate approaches; **the choice is deferred until this is actually scheduled** (revisit when a store routinely has more than ~3 alerts, or sub-second alerting becomes a hard requirement):
  - **Option A — in-process pub/sub event bus.** The store's write path publishes records; workers subscribe instead of polling. Eliminates polling entirely (~0ms latency, no cursor files) at the cost of write-path changes and a larger blast radius.
  - **Option B — one shared poll loop per store.** Collapse N per-alert loops into a single `StoreEvaluator` that scans once per tick and fans records out to per-alert evaluators. Alerts-internal change only (no write-path changes), but keeps the up-to-1s latency and needs decisions on poll-interval reconciliation, per-alert filter handling, and a single per-store cursor.

  Per-alert sinks, cooldowns, and dispatch semantics stay unchanged in either approach.
- **Retry policies for webhook**: today, transient HTTP failures are not retried. A bounded exponential backoff inside `notify.Webhook` would be cheap to add.
- **TLS / mTLS for MQTT**: `tcp://` brokers only. `ssl://` (TLS) and `wss://` (MQTT-over-WebSocket) need config knobs.
- **Authentication on MQTT-over-WSS**: combine broker `wss://` URL with header auth for cloud-broker scenarios.
- **Per-alert metrics**: `alertsFired` is exposed in status but there's no histogram of latency, drop counts, or per-rule fire counts. Worth surfacing if alerts become widely used.
