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
   - **Staleness rules are evaluated on every tick**, including both early-out paths, using that same newest timestamp. Steps 2–5 below describe the `condition` path only; a staleness rule skips them entirely and goes straight to the cooldown + dispatch of step 5. See [Staleness rules on the shared loop](#staleness-rules-on-the-shared-loop).
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

### Staleness rules on the shared loop

Staleness (issue #134) is the one rule type that must run on a tick where *nothing arrived*, which is exactly the tick the idle early-out was built to skip. The decisions that follow from that:

| Decision | Choice | Why |
|---|---|---|
| Where the check runs | On **every** tick, before both early-out returns and on the scan path | An idle tick is the only tick a staleness rule exists for; returning early without checking would make the rule type inert |
| Cost | Reuses the `GetNewestTimestamp()` the early-out already computes, passed into the check | O(partitions) of metadata, already read once per tick — a staleness rule adds one comparison and **no** extra read, scan, or per-record state |
| Shared cursor | Staleness workers are excluded from the scan-position floor on register | They consume no records; letting one pull the scan back would make every other alert replay a range it already handled |
| Scan suppression | A store whose *only* alerts are staleness rules skips the range read entirely | Such a store never advances the shared cursor, so `newestTs <= lastTs` would never hold and every tick would block-scan the whole store |
| Tick interval | Staleness workers still count toward `min(poll_interval)` | They need ticks to fire; excluding them would let a slow condition alert delay staleness detection |
| Fan-out | `deliverBatch` is a no-op for staleness workers | Their evaluator is used only for cooldown + dispatch; it never evaluates record contents |
| Scan errors | Staleness is skipped when the newest-timestamp read fails | Without a trustworthy timestamp there is no age to judge; the scan error already marks every worker `state="error"` |

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
| `rule_type` | no | `"condition"` (default) or `"staleness"`. See [Rule types](#rule-types). |
| `condition` | for `condition` rules | Rule expression, e.g. `temperature > 80`. Rejected for staleness rules. |
| `max_age` | for `staleness` rules | Fire when nothing has arrived for this long (duration string, e.g. `5m`). No default. Rejected for condition rules. |
| `cooldown` | no | Min time between fires (duration string, e.g. `5m`). |
| `external_ref` | no | Opaque pass-through string (≤512 bytes, no NUL); echoed verbatim on every alert payload. |
| `message` | no | Template rendered into the payload's `message` field on each fire (≤512 bytes, no NUL). See [Message templates](#message-templates). |
| `restart_policy` | no | `"now"` (default — start from wall-clock now, no cursor) or `"resume"` (replay from cursor on restart). |
| `max_replay` | no | Duration cap on resume replay window. Only valid when `restart_policy="resume"`. Default: unbounded. |
| `poll_interval` | no | Poll cadence hint (default `1s`). The store's shared loop ticks at the **minimum** across its alerts, so a faster alert speeds up every alert on the store and a slow value only takes effect if it is the minimum. |

`condition` syntax: `field <op> value`, optionally compounded with `AND` / `OR`. Operators: `==`, `!=`, `>`, `>=`, `<`, `<=`, `contains`. Values: numbers, quoted strings, booleans. Field names may include dots and hyphens (`cpu.percent`, `temp.cpu_c`).

### Rule types

`rule_type` selects **what makes the alert fire**. It defaults to `"condition"`, so every alert created before this field existed keeps working unchanged — no migration.

| Rule type | Fires on | Driven by |
|---|---|---|
| `condition` (default) | A record arrived whose fields satisfy `condition` | Record contents |
| `staleness` | No record has arrived for longer than `max_age` | The poll tick / wall clock |

**Why staleness cannot be a condition.** A condition is a function of a record's fields. Absence has no record and therefore no fields to write an expression against, so no operator can express it. That makes staleness a distinct rule type rather than a new operator (issue #134).

The failure this covers is a collector that is OOM-killed, whose host reboots, or whose network partitions. It produces exactly zero records, so a condition rule — which only ever runs against records that arrived — produces exactly zero alerts, forever. Detection has to live in the store, because a collector reporting its own death only works while it is alive.

```json
{
  "type": "webhook",
  "rule_type": "staleness",
  "name": "nas-syn-002-disks went quiet",
  "max_age": "5m",
  "cooldown": "30m",
  "webhook": { "url": "https://hooks.example.com/incoming" }
}
```

Semantics:

- **`max_age` is per-rule and opt-in, with no implicit default.** A collector polling every 60s should alert after a few missed polls, but an event-driven source (a door contact) can be legitimately silent for days. Any global default would flood one of those two cases, so there isn't one.
- **An empty store never fires.** A store that has never received a single record is "not yet started", not stale — otherwise every newly created store alerts before its collector's first write. Only a store that *had* data and stopped can go stale.
- **The age is floored at the worker's start time.** An alert created against a store that went quiet last week does not fire immediately on a gap that predates it.
- **No resolve event.** When data returns, the rule simply stops firing; `cooldown` bounds repeat fires while the store stays quiet. Adding a resolve signal would mean a new payload field that is meaningless for every condition alert, so it is deliberately left for a future change that can design it across *all* rule types.
- **`condition`, `restart_policy: "resume"`, and `max_replay` are rejected** (400) for staleness rules rather than silently ignored. A staleness rule has no cursor to resume from and no record to evaluate, so accepting these would misrepresent what the alert does.

The alert payload uses the same shape as any other alert. Because there is no triggering record, `data` describes the absence instead, and `timestamp` is the newest record's timestamp — the moment data stopped, which is what a receiver wants to display:

```json
{
  "rule_name": "nas-syn-002-disks went quiet",
  "condition": "no data for 5m0s",
  "timestamp": 1747000000000000000,
  "data": {
    "last_timestamp": 1747000000000000000,
    "age_seconds": 612.4,
    "max_age_seconds": 300,
    "store": "nas-syn-002-disks",
    "rule_type": "staleness"
  },
  "store_name": "nas-syn-002-disks"
}
```

Staleness reuses everything already built — the three sinks, cooldown, `external_ref`, the persisted last-fired mark — so a staleness alert appears in the same `GET /alerts` list and the same dashboard table as threshold alerts, tagged by `rule_type`.

**Scope: per-store, not per-series.** This fires on "the store stopped receiving", which catches a dead collector. It does *not* catch one series going quiet while others keep reporting — on a tall store, four disks still writing keeps the store-level newest timestamp fresh. That case needs `latest_by`-style grouping and is tracked separately in issue #135.

## HTTP API

All endpoints require the store's `X-API-Key` (same as streaming endpoints). The two GETs require the `read` access class; creating, updating, testing, and deleting alerts require `manage` — so a dashboard watching rule health needs no administrative authority. Read-tier detail is safe because the payload redacts every credential surface: sink URLs lose userinfo and query strings, MQTT passwords are masked, and header values are masked.

| Method | Path | Body | Description |
|---|---|---|---|
| POST   | `/api/stores/{store}/alerts` | `CreateAlertRequest` | Create an alert. The `type` field discriminates webhook / ws / mqtt. |
| POST   | `/api/stores/{store}/alerts/test` | `{condition, data}` | Dry-run a condition against a sample record — returns how it parsed and whether it matched. No alert is created. |
| GET    | `/api/stores/{store}/alerts` | — | List all alerts (all three types, tagged with `type`). |
| GET    | `/api/stores/{store}/alerts/{id}` | — | Get one alert's runtime status + persisted config (secrets redacted). |
| PUT    | `/api/stores/{store}/alerts/{id}` | `CreateAlertRequest` | Update an alert in place. Same body shape as create; the path id wins. See [Updating an alert](#updating-an-alert). |
| DELETE | `/api/stores/{store}/alerts/{id}` | — | Stop the worker and remove the persisted config. |

#### Updating an alert

`PUT /api/stores/{store}/alerts/{id}` edits an alert without destroying it. The alert **id**, its `created_at`, its poll cursor, and the worker's fired counter all survive — which is exactly what delete-and-recreate loses.

Two things are deliberately *not* carried over unchanged, because an edit is a reconfiguration rather than a restart (issue #179):

- **The cooldown mark is cleared when `cooldown` changes.** It persists across process restarts so a still-closed window survives a bounce, but an operator shortening a cooldown is explicitly asking for the old window not to apply. An edit that leaves `cooldown` alone keeps the mark, so renaming a rule or moving its sink URL will not fire it immediately.
- **The staleness grace period is not re-armed.** A worker's start time floors the age a staleness rule measures, so that a newly created alert does not fire on a gap predating it. A replacement worker inherits its predecessor's start time; otherwise every edit would silence a staleness rule for a full `max_age` — precisely when the operator is tuning it and watching for the next fire.

Semantics are **full replace**, matching create: a field omitted from the body reverts to its default (omit `cooldown` and the alert has no cooldown). The one exception is the credentials reads redact — auth-style header values and the MQTT password. A client that fetched the alert only ever saw `[redacted]`, so it cannot round-trip them; submitting the marker back (or omitting the password) keeps the stored value. Submitting a real value replaces it, so rotation still works.

The header **map** is replaced wholesale even though individual redacted values are preserved: dropping a header from the map removes it. If a `[redacted]` marker arrives under a header name that isn't stored, the value is dropped rather than persisted literally.

Changing an alert's `type` is rejected with `400` — the persisted resources are per-transport, and a swap is rare enough to be a delete plus create. Validation is identical to create, and a rejected update leaves the running alert untouched.

#### What "secrets redacted" covers

Read responses never echo credential material. The on-disk config is unchanged — this shapes only what the API returns:

- **Sink URLs** (webhook/WS URL, MQTT broker URL, and the `target` field in listings): userinfo is stripped and any query string is replaced with `[redacted]`, since both commonly carry bearer secrets. Scheme, host, and path survive so the target stays identifiable.
- **MQTT password**: masked.
- **Headers**: masked by **allowlist** — every header value is `[redacted]` except a small set of benign transport headers (`accept`, `content-type`, `user-agent`). This is deliberately not a list of known-secret names: header names are operator-chosen, so a denylist misses vendor auth headers like `X-Vendor-Signature`, `X-Hub-Signature-256`, or `PRIVATE-TOKEN`. Failing closed costs only the ability to read back a benign custom header's value.

An empty header value passes through unmasked — `[redacted]` there would falsely imply a value exists.

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

The server rejects requests where `type` is missing or unknown, where the nested options block for the chosen `type` is absent, or where any of the *other* transport blocks are also set. `max_replay` without `restart_policy="resume"` is rejected. `external_ref` over 512 bytes or containing NUL bytes is rejected, as is `message`. Templates are **not** checked for whether the fields they reference exist — see [Message templates](#message-templates).

Rule-type validation: an unknown `rule_type` is rejected. For `rule_type="condition"` (the default), `condition` is required and `max_age` is rejected. For `rule_type="staleness"`, `max_age` is required and must parse to a positive duration, while `condition`, `restart_policy="resume"`, and `max_replay` are all rejected — a staleness rule has no cursor and no record, so accepting those fields would leave the persisted config describing behavior the alert does not have.

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
  "external_ref": "dashboards/warehouse-sensors#component-42",
  "message": "Server Room 5 is at 95.0C (limit 80)"
}
```

`data` is the full record that triggered the match. `external_ref` and `message` are omitted when the rule didn't configure one.

#### `external_ref` — opaque pass-through

Each alert may carry an `external_ref` string (≤512 bytes, no NUL bytes, otherwise unconstrained). ts-store does not parse or interpret it — receivers can stash whatever they need: a dashboard component id, a Grafana slug, a Slack channel name, or a JSON-encoded compound key like `{"dashboard_id":"…","namespace":"default"}`. When the alert fires, the value is echoed verbatim on the alert payload above. If the alert didn't set one, the field is omitted from the JSON.

### Message templates

Nothing else in the payload is a sentence. Without a template, every receiver — a Slack webhook, a notification service, the dashboard's bell row — assembles its own text from `rule_name` / `condition` / `data`, so the same formatting gets reimplemented per consumer and drifts. A per-rule `message` fixes it once, where someone already knows what the rule means.

```json
{
  "name": "high temp",
  "condition": "temperature > 80",
  "message": "Server Room 5's temperature {temperature} has exceeded the max"
}
```

fires as `"message": "Server Room 5's temperature 95 has exceeded the max"`.

#### Variables

| Variable | Value |
|---|---|
| `{store}` | Store name |
| `{rule_name}` | The rule's name |
| `{condition}` | Rendered condition (`temperature > 80`, or `no data for 5m0s`) |
| `{timestamp}` | The alert's timestamp (epoch nanoseconds) |
| `{external_ref}` | The rule's opaque pass-through, when set |
| `{<field>}` | Any field of the triggering record, by its own name |

Staleness rules have no triggering record, so their fields come from the synthetic absence data: `{age_seconds}`, `{max_age_seconds}`, `{last_timestamp}`, `{store}`, `{rule_type}`.

```
"{store} went quiet — no data for {age_seconds:.0f}s (limit {max_age_seconds:.0f}s)"
  → "nas-syn-002-disks went quiet — no data for 612s (limit 300s)"
```

The built-ins are **reserved**: a record field named `store` or `timestamp` is shadowed by the built-in. Narrow risk, and the alternative — record fields shadowing built-ins — would let a template silently change meaning when a new field appears in the data.

#### Format specs

`{field:spec}` applies an optional conversion:

| Spec | Effect | Example |
|---|---|---|
| `.Nf` | Fixed decimal places (N ≤ 10) | `{temp:.1f}` → `95.0` |
| `time` | Epoch integer as RFC3339 **UTC** | `{timestamp:time}` → `2025-05-11T21:46:40Z` |

A spec that doesn't apply to the value is **ignored** rather than erroring — `{name:.2f}` on a string renders the string. A receiver reading `temperature nas-01` can tell something is off; one reading `%!d(string=nas-01)` learns nothing and the operator gets a corrupted page.

`{:time}` infers the unit from magnitude (seconds / milliseconds / microseconds / nanoseconds), since a user's own field may hold any of them. The documented cost: a genuine *nanosecond* value from the first ~100 seconds after the epoch is read as seconds. ts-store never produces one.

Output is always UTC. A container without `TZ` set already runs UTC, so "server local" is frequently UTC in disguise, and the same instant would otherwise stamp differently across hosts — painful exactly when correlating one incident across a fleet.

#### Rendering rules

- **Unknown or misspelled field renders empty, and never fails the alert.** `{temprature}` produces `""`, the surrounding text still renders, and the alert still fires. A formatting mistake must not suppress the thing it was describing.
- **`{{` and `}}`** are literal braces. An unclosed `{` is emitted literally rather than swallowing the rest of the message.
- **Field existence is not validated at create time.** For JSON stores the field set isn't known until data arrives, so validation would either reject valid templates or give false confidence.
- **Numbers never render in scientific notation.** A large integer-valued float renders `1747000000000000000`, not `1.747e+18`.
- **Nested values render as compact JSON.** Braces in that output are literal — rendering is single-pass and never re-scans its own output for placeholders.
- **Template source is capped at 512 bytes**, matching `external_ref`. The cap is on the source, not the rendered output, since the rendered length varies per fire with the data.

The `message` field is **additive**: it is omitted entirely when a rule sets no template, so existing receivers see a byte-identical payload.

## CLI

The CLI mirrors the HTTP API. Each transport has its own `add` subcommand, but all share the same rule + dispatch flags (`--name`, `--rule-type`, `--condition`, `--max-age`, `--cooldown`, `--external-ref`, `--message`, `--restart`, `--max-replay`, `--poll`, `--api-key`).

```sh
# Webhook alert
tsstore alerts webhook add <store> --url <url> \
  --name high-temp --condition "temperature > 80" --cooldown 5m \
  [--header Authorization:Bearer\ xyz] [--timeout 10s] \
  [--restart now|resume] [--max-replay 1h] [--external-ref <s>] \
  [--message "<template>"] \
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

# Staleness alert — fire when the store STOPS receiving data.
# Works with any transport; takes --max-age instead of --condition.
tsstore alerts webhook add <store> --url <url> \
  --name "collector went quiet" \
  --rule-type staleness --max-age 5m --cooldown 30m

# Dry-run a condition against a sample record (no alert created).
# Condition rules only — a staleness rule has no record to dry-run against.
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
- **Per-series staleness**: issue [#135](https://github.com/trv-enterprises/ts-store/issues/135). The per-store rule above misses one series going quiet while others keep reporting — four disks still writing keeps the store's newest timestamp fresh. Needs `latest_by`-style grouping and an authored `series_field`/`series_value`, since the store cannot infer which series *ought* to exist.
- **Resolve / clear events**: staleness alerts stop firing when data returns but emit no explicit "recovered" signal. Worth designing across all rule types rather than only staleness, since a condition alert has the same gap.
- **Retry policies for webhook**: today, transient HTTP failures are not retried. A bounded exponential backoff inside `notify.Webhook` would be cheap to add.
- **TLS / mTLS for MQTT**: `tcp://` brokers only. `ssl://` (TLS) and `wss://` (MQTT-over-WebSocket) need config knobs.
- **Authentication on MQTT-over-WSS**: combine broker `wss://` URL with header auth for cloud-broker scenarios.
- **Per-alert metrics**: `alertsFired` is exposed in status but there's no histogram of latency, drop counts, or per-rule fire counts. Worth surfacing if alerts become widely used.
