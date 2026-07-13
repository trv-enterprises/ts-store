# Alerting Architecture

This document describes the design of the ts-store alerting system since issue [#4](https://github.com/trv-enterprises/ts-store/issues/4) landed (post-v0.15.0). Each store runs **one shared poll loop** that scans once per tick and fans new records out to every alert's evaluator (see [Architecture](#architecture) and [Shared poller design](#shared-poller-design)); before #4, every alert ran its own polling worker, so N alerts on one store cost N redundant scans — and N reads and N JSON parses of the same records — per tick.

## Overview

Alerting is a first-class, transport-independent feature. Each **alert** carries exactly one **rule** — a condition over the fields of a stored record (e.g. `temperature > 80`) — and a **sink** that dispatches a payload when the rule matches. Three sink types are supported, each as its own on-disk resource:

| Resource | Sink | Use case |
|---|---|---|
| `webhook_alerts` | HTTP POST to a URL | PagerDuty, Slack, custom hooks |
| `ws_alerts` | Open-on-fire outbound WebSocket frame | Existing WS consumers |
| `mqtt_alerts` | Publish to an MQTT topic (QoS 1) | Broker fan-out to many subscribers |

All three are persisted per-store as JSON and survive daemon restarts (re-loaded by the manager on boot). Each alert owns its evaluator goroutine and sink; record delivery comes from the store's single shared poller.

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
            │  │ ONE shared scan per tick (min of the alerts' poll_intervals)
            │  │
┌───────────┼──┼─────────────────────────────────────────────────────────────┐
│ alerts.Manager (per-store)                                                 │
│           │  │                                                             │
│   ┌───────▼──┴────────┐                                                    │
│   │ alerts.poller     │  One per store. Scans once from the minimum of     │
│   │ (shared loop)     │  the workers' cursors, reads + expands + parses    │
│   │                   │  each record once, fans the batch out.             │
│   └───────┬───────────┘                                                    │
│           │ deliverBatch(records)   (one shared read-only parsed map)      │
│           ▼                                                                │
│   ┌───────────────────┐                                                    │
│   │ alerts.Worker     │  One per configured alert resource. Filters the    │
│   │                   │  batch by its own cursor, feeds eligible records   │
│   │                   │  into its Evaluator.                               │
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

1. **Polling**: the store's shared poller calls `store.GetObjectsInRange(lastTs+1, now, 100)` once per tick, where `lastTs` is the shared scan cursor and the tick is the **minimum `poll_interval` across the store's alerts** (each alert's value is a hint; the fastest wins for the whole store). An idle tick — nothing written since the cursor — early-outs on the store's newest timestamp without scanning (issue #57).
2. **Read + expand + parse, once**: each scanned record is read from storage once; for schema-type stores it is expanded via `store.ExpandData` so condition fields resolve to schema field names, then JSON-parsed. The resulting map is fanned out **read-only** to every worker — rule evaluation and sink payload marshalling never mutate it.
3. **Fan-out**: each worker filters the batch by its own cursor, so a record is evaluated at most once per alert even when the shared scan re-covers old ranges — e.g. when a `resume`-policy alert registers with a persisted cursor behind the shared position, the scan is pulled back and only that alert replays the backlog.
4. **Evaluation**: eligible records are handed to `rules.Evaluator.Evaluate(ts, data)`, which buffers into a 1000-element channel; a full channel drops the record and increments the alert's `records_dropped` counter, so one slow sink can't stall the shared loop.
5. **Match → dispatch**: in the evaluator's goroutine, the rule is checked; on a match, cooldown is enforced; on first-allowed match, the callback fires `Sink.Send(alert)`.
6. **Cursor & restart policy**: each alert configures its own `restart_policy` (default `"now"`):
   - `"now"` — the worker starts from `time.Now()` on every Start and skips cursor writes entirely. Best for high-frequency metric streams where a brief gap is acceptable.
   - `"resume"` — the worker reads `<store>/<type>_alert_<id>.cursor` on Start and replays records since its cursor (via the shared-scan pullback in step 3). If `max_replay` is set, the resume window is bounded to `now - max_replay` to cap the work after long outages. Best for event streams (e.g. journal logs) where a missed match is meaningful. Cursors stay **per-alert files**, written once per delivered batch — no migration was needed when the shared poller landed.

## Shared poller design

The decisions locked when #4 landed (Option B — shared poll loop; Option A, a write-path event bus, was the rejected alternative, kept in [Future work](#future-work)):

| Decision | Choice | Why |
|---|---|---|
| Tick reconciliation | `min(poll_interval)` across the store's alerts; per-alert field kept as a hint | Work-conservative — a slow alert never delays a fast one; recomputed on every add/remove |
| Cursors | Per-alert cursor files kept, written once per delivered batch | Preserves per-alert restart semantics exactly; zero migration. Only the in-memory shared scan position is new |
| Replay | Registering a worker whose cursor is behind the shared scan pulls the scan back | Caught-up workers skip the re-covered range via their own cursor filter, so replay is idempotent per alert |
| Backpressure | Fan-out is non-blocking: a full evaluator channel drops the record and counts `records_dropped` | One slow sink must not stall the shared loop or other alerts |
| Error propagation | A shared scan error sets `state="error"` on **every** registered alert; the next clean pass restores them | There is one scan now — its failure is every alert's failure |
| Lifecycle | The loop starts lazily on the first alert and runs until the manager stops; a tick with zero workers returns before touching the store | No start/stop races on add/remove churn; idle cost is one timer wake-up |
| Corrupt records | Still advance every cursor (delivered with a nil parsed map) | A single bad record must not stall alert evaluation forever |

Cost model (N alerts on one store): scans per tick N → **1**; per-record read + schema-expand + JSON-parse N → **1**; per-alert evaluation and dispatch unchanged. Latency is unchanged (up to the store's effective tick) — sub-second delivery is Option A territory.

## Configuration

### File layout

Per store, the on-disk layout includes:

```
<store>/
  meta.tsdb
  partition-N/...
  ws_connections.json      ← streaming WS push
  mqtt_connections.json    ← streaming MQTT sink
  webhook_alerts.json      ← persisted webhook alerts
  ws_alerts.json           ← persisted WS alerts
  mqtt_alerts.json         ← persisted MQTT alerts
  *_alert_<id>.cursor      ← per-alert cursor (resume-policy alerts only)
  *_alert_<id>.lastfired   ← per-alert cooldown mark (all policies)
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
| `poll_interval` | no | Poll cadence hint (default `1s`). The store's shared loop ticks at the **minimum** across its alerts, so a faster alert speeds up every alert on the store and a slow value only takes effect if it is the minimum. |

`condition` syntax: `field <op> value`, optionally compounded with `AND` / `OR`. Operators: `==`, `!=`, `>`, `>=`, `<`, `<=`, `contains`. Values: numbers, quoted strings, booleans. Field names may include dots and hyphens (`cpu.percent`, `temp.cpu_c`).

## HTTP API

All endpoints require the store's `X-API-Key` (same as streaming endpoints).

| Method | Path | Body | Description |
|---|---|---|---|
| POST   | `/api/stores/{store}/alerts` | `CreateAlertRequest` | Create an alert. The `type` field discriminates webhook / ws / mqtt. |
| POST   | `/api/stores/{store}/alerts/test` | `{condition, data}` | Dry-run a condition against a sample record — returns how it parsed and whether it matched. No alert is created. |
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

Sink URLs are parsed at create time and must carry a host and a scheme matching the transport — `http`/`https` for webhooks, `ws`/`wss` for WebSocket sinks, and one of paho's accepted schemes (`tcp`, `ssl`, `tls`, `ws`, `wss`, `mqtt`, `mqtts`, `unix`) for MQTT broker URLs. A malformed or mis-schemed URL is a 400 at create rather than a runtime `last_error` at first fire.

**SSRF posture.** Loopback, link-local, and private-range sink targets are deliberately *not* blocked: alerts posting to services on the same box or LAN are a core homelab pattern here. The consequence is that anyone holding a store API key can make the daemon POST to any address the daemon can reach. Treat store API keys accordingly on shared or internet-exposed deployments; a deny-list (e.g. blocking `169.254.0.0/16`) can be revisited if that threat model ever applies.

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

# Dry-run a condition against a sample record (no alert created)
tsstore alerts test <store> --condition "temperature > 80" --data '{"temperature": 95}'

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
| Webhook returns non-2xx | Logged; alert is *not* retried. Surfaced as `delivery_failures` + `last_error` in status. |
| Webhook queue full (100) | Alert dropped, logged. Indicates downstream is too slow. |
| WS dial fails | Single-alert failure, logged. Worker continues; next match retries the dial. |
| MQTT publish fails | Worker logs and continues. The persistent client will auto-reconnect on its own. |
| Shared scan fails (range read error) | Every registered alert flips to `state="error"` with the scan error as `last_error`; the next clean pass restores `"running"`. |
| Evaluator channel full (1000) | Record dropped for that alert only, counted in `records_dropped`. Other alerts and the shared loop are unaffected. |
| Daemon crash mid-poll | If `restart_policy="now"` (default), the worker starts from "now" on restart and any in-flight matches are lost. If `restart_policy="resume"`, the worker replays from the last persisted cursor (optionally bounded by `max_replay`). |
| Bad rule syntax in config | Worker construction fails at load time; logged; other alerts continue. |
| Corrupt record on disk | All cursors advance past it; it is never evaluated. |

## Future work

- **Sub-second alerting (event bus)**: issue [#4](https://github.com/trv-enterprises/ts-store/issues/4) landed its Option B — the shared poll loop described above — which removed the N×scan cost. The alternative it weighed (Option A, an in-process pub/sub bus from the write path) remains the path to ~0ms write→evaluate latency if that ever becomes a requirement; polling latency today is up to the store's effective tick.
- **Retry policies for webhook**: today, transient HTTP failures are not retried. A bounded exponential backoff inside `notify.Webhook` would be cheap to add.
- **TLS / mTLS for MQTT**: `tcp://` brokers only. `ssl://` (TLS) and `wss://` (MQTT-over-WebSocket) need config knobs.
- **Authentication on MQTT-over-WSS**: combine broker `wss://` URL with header auth for cloud-broker scenarios.
- **Per-alert metrics**: `alertsFired` is exposed in status but there's no histogram of latency, drop counts, or per-rule fire counts. Worth surfacing if alerts become widely used.
