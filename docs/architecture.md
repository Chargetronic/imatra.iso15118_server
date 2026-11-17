# Architecture

This document describes one **deployment** of the server: what it is made
of, how a charging session flows through it, what it relies on, and where its
limits are. Decisions behind it are recorded in [adr/](adr/).

## Context

```
 EV ──PLC── device at the charger ──MQTT/TLS (8883)── deployment ──HTTPS /v1── operator systems
                    │                                     ▲                    (CMS, DERMS, VPP,
                    │ daily: certificate, binding          │ registry           OpenADR adapter)
                    └──────────── service layer ───────────┘ (MQTT, registry/#)
```

* **The device** terminates the PLC link and, where the EV uses it, TLS
  towards the EV. It carries no ISO 15118 session logic: it tunnels the EV's
  V2G messages to the deployment and answers the repetitive Charging Loop
  from a cache pair (request → response) that the engine gives it, with a TTL.
* **The deployment** runs the protocol for all its devices, holds the state of
  every plug-in, applies the operator's limits and activations, and keeps the
  record of what happened.
* **The service layer** (optional, not part of this repository) issues device
  identities and decides which operator a device belongs to. It publishes the
  registry into the deployment's broker and never calls its API
  ([registry.md](registry.md)). Without it, the deployment runs its own CA and
  the operator keeps the registry.
* **Operator systems** use only `/v1` and the event feed.

## Components

| Component | Role | State |
|---|---|---|
| Mosquitto (MQTT 5) | device connections; admission by client certificate (CN = device id); ACL: a device sees only `dev/<id>/#`, the layer only `registry/#` | retained messages, persistent sessions |
| engine (Go, N instances) | ISO 15118-2 AC (EIM), DIN 70121 SoC read-out, IEC 61851-1 PWM; cache pairs; command confirmation; activation executor; registry intake (leader) | none of its own: everything in Valkey |
| Valkey | working state: digital sessions, station settings and the limit in force, plug-in records, the activation pool, the event stream | AOF every second, `noeviction` |
| relay (Go) | event stream (Valkey) → `domain_events` (Postgres), at least once, deduplicated by event id | cursor in Postgres |
| projector (Go) | operator tables from the feed: devices, plug-in sessions, attributes; rebuildable from scratch | Postgres |
| Postgres | events, operator tables, operator commands, keys and audit | the record |
| API (Python, FastAPI) | `/v1`: reading, limits, activations, device settings, local block, event feed; keys with rights; audit; OpenAPI 3.1 | Postgres |
| gateway (Caddy) | TLS for `/v1` and `/health`, rate limit per operator key | its certificates |
| pki-init | the deployment's CA and broker certificate, or import of the files from the service layer | `pki` volume |

Engines share the device topics through MQTT 5 shared subscriptions; any
instance can serve any message of any device. Order of one device's messages
and the time of events are handled as in [ADR-001](adr/adr-001-device-message-order.md)
and [ADR-003](adr/adr-003-server-time.md): event time is the server's, the
device gives identifiers and durations.

## A charging session

1. The cable is plugged in: the device reports the control pilot state; the
   engine opens a plug-in record.
2. For a digital session, the device tunnels the EV's V2G messages
   (`dev/<id>/v2g/up`); the engine answers on `dev/<id>/v2g/down`. The EV's
   energy request, departure time, maximum current and, from DIN 70121, the
   state of charge become events and attributes.
3. In the Charging Loop the engine pushes a cache pair to the device
   (`dev/<id>/cl/push`); the device answers the EV from it and forwards one hit
   in a sampling period. A new limit or schedule is a new pair.
4. Without a digital session the engine controls the charging current by PWM
   (`dev/<id>/cp/set`).
5. Limits and activations from `/v1` are stored in Postgres before they are
   sent; the engine applies them from Valkey and reports the device's
   confirmation (`dev/<id>/cmd/ack`).
6. On unplug, the plug-in record is closed with its energy; the session
   appears in `/v1/sessions` and the feed.

Nothing on the path of an answer to the EV waits on Postgres
([ADR-002](adr/adr-002-working-state-in-valkey.md)); ISO 15118-2 gives the EV a
2-second timeout for most messages.

## Interfaces (contracts)

| Interface | Defined in |
|---|---|
| Device protocol `dev/<id>/…` | `engine/internal/tunnel` |
| Registry and business rules | [registry.md](registry.md), [rules.md](rules.md) |
| Operator API `/v1` | `GET /v1/openapi.json` |
| Event feed | `api/schemas/events.v1.json` |
| Settings | `.env.example` |
| Metrics and probes | [metrics.md](metrics.md) |
| Database schema | `api/migrations` |

## Configurations

| | Reference |
|---|---|
| Deployment | one host, Docker Compose, two engines (`--scale engine=2`) |
| Hardware | 2 vCPU, 8 GB, NVMe |
| Stated capacity | 5 000 devices with 3 200 charging sessions at once, on 2 dedicated vCPU / 8 GB (measured, see below) |
| Measured (16–17.11.2026) | 2 dedicated vCPU / 8 GB server, load generator on another host; 5 000 devices admitted by client certificate (mTLS) of a service layer, registry bound; 3 200 EVs (70 % ISO 15118-2, 30 % PWM) plugged in within 3 minutes (about 18 a second), charging 10 minutes |
| V2G answers | 0 timeouts in steady operation and in a reconnect of the whole fleet; p99 0.06–0.8 s, max < 2 s. A few timeouts (2–18 a run) only when all 5 000 devices connect at 200 a second and the EVs plug in at the same moment |
| Activation stage | 2 000 devices: 95 % confirmed within 7.3 s, all within 8.1 s; 3 000 devices: all within 16 s (the CPU is the limit, about 250 devices a second) |
| Reconnect | all 5 000 devices back within 61 s with 3 200 sessions charging; charging goes on from the Charging Loop cache |

One engine is allowed and not measured separately.

**Need more?** More cores on the same host; a second engine on another host
(allowed by the design, not measured); several deployments under one
management, redundancy and automatic scaling are the commercial editions.

## Assumptions

| Assumption | Value |
|---|---|
| Fleet | the design is sized for 20 000 devices, one session a day each; this edition is stated for 5 000 on 2 vCPU (measured) |
| Simultaneity | at night 60–70 % plugged in; in the evening half the fleet plugs in within 2 h |
| Vehicles | about 70 % ISO 15118-2 AC, 30 % PWM only |
| Telemetry | every 60 s and on a change of current by 1 A; Charging Loop sample every 30 s; heartbeat every 60 s |
| Link | LTE; the server does not depend on device clocks |
| Server time | synchronised (NTP) |

## Limits

Single components, no redundancy in this edition:

| Component down | Effect |
|---|---|
| one of two engines | the other takes over; the leader changes within 5–8 s |
| the only engine (minimal) | answers to EVs stop until restart; devices answer from the cache until its TTL |
| Valkey restarted with AOF | up to ~1 s lost; sessions continue; a gap in the feed is marked |
| Valkey lost completely | running digital sessions end with `FAILED_UnknownSession` and start again; limits and the pool are rebuilt from Postgres; a car in the Charging Loop may not get a new limit until the cache pair expires |
| Postgres | answers to EVs continue; the API is unavailable; the feed catches up |
| broker | all devices reconnect (target ≤ 5 min) |
| relay, projector | operator data lags, then catches up without loss or duplicates |

Protocol and data:

* `v2g/up` is QoS 0: a request given to an engine that fails is lost; the EV
  repeats it or reconnects.
* Operator tables lag the feed: target ≤ 2 s, up to 5 s.
* Power values are informational, not for billing.
* Mass events set the load limit: about 3 000 sessions stopping in the same
  second cause `SessionStop` timeouts. An activation step addresses up to
  2 000 devices; a larger pool is served in waves.
* Scaling is manual; only engines can be updated without downtime.
* API authentication is by key only. Backups are the operator's
  responsibility (`scripts/export.sh`).

Admission and registry:

* Unbinding and revocation arrive as registry messages; the device
  certificate (7 days when issued by the service layer) is the backstop.
* A change of operator takes effect at the user's midnight and not before the
  cable is unplugged ([registry.md](registry.md)).
