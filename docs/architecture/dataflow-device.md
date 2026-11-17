# Data flow: device ↔ server

How a device (an adapter between a charging station and an EV, or a module
inside a station) exchanges data with the EV and with the server of one
deployment.

- **Contract (source of truth):** [`spec/asyncapi-device.yaml`](../../spec/asyncapi-device.yaml)
  — AsyncAPI 3.0: topics, QoS, retain, payload schemas, enumerations, ports.
  This page shows the flows; field-level detail lives in the contract.
- **Code:** `engine/internal/tunnel` (message types), `engine/cmd/engine`
  (handlers), `engine/cmd/fleetgen` (a virtual device speaking the same protocol).
- **Related:** [registry.md](../registry.md) (service layer → server),
  [rules.md](../rules.md), [ADR-001](../adr/), ADR-003 (server time).

Other boundaries: [inside the deployment](dataflow-internal.md),
[service layer ↔ deployment](dataflow-service.md).

## 1. Overview

```mermaid
flowchart LR
  subgraph site["Charging site"]
    EVSE["Charging station<br/>IEC 61851-1"]
    EV["EV<br/>ISO 15118-2 AC / DIN 70121 / PWM only"]
    DEV["Device<br/>thin proxy: SLAC, SDP, V2GTP/TLS,<br/>Charging Loop cache, CP mask, metering"]
  end

  subgraph dep["Deployment (one operator)"]
    BRK["MQTT broker<br/>Mosquitto 2"]
    ENG["Engine × N<br/>Go + libcbv2g, stateless"]
    VK[("Valkey<br/>working state")]
    PG[("Postgres<br/>commands, feed, history")]
    API["/v1 API"]
  end

  LAYER["Service layer<br/>(identity, certificates,<br/>TLS signing)"]

  EVSE -- "CP: PWM 1 kHz ±12 V<br/>(station limit)" --> DEV
  DEV -- "CP: mask 5 % / limit / transit<br/>IEC 61851-1" --> EV
  EV <-- "PLC HomePlug GreenPHY<br/>SLAC (ISO 15118-3)<br/>SDP UDP :15118<br/>V2GTP over TCP or TLS 1.2, EXI" --> DEV

  DEV <-- "MQTT over TLS :8883<br/>mTLS, CN = device id<br/>JSON, topics dev/&lt;id&gt;/…" --> BRK
  BRK <-- "MQTT :1883 (internal)<br/>$share/engine/dev/+/…" --> ENG
  ENG --- VK
  ENG --- PG
  API --- PG

  DEV -. "separate short mTLS connections:<br/>certificate renewal, TLS signature" .- LAYER
  LAYER -. "registry/dev/&lt;id&gt; (see registry.md)" .-> BRK
```

The device does not decode EXI and holds no ISO 15118 logic, limits or
history. The engine answers every EV request except the repetitive Charging
Loop, which the device answers from a **cache pair** (request bytes →
response bytes) the engine gives it. Session traffic never passes through the
service layer.

## 2. Topics

All topics are `dev/<id>/…`, where `<id>` is the CN of the device certificate.
The broker's ACL lets a device read and write its own subtree only.

**Device → server**

```mermaid
flowchart LR
  D1["Device"]
  E1["Engine"]
  u1["<b>v2g/up</b> · QoS 0<br/>Up: conn, seq, exi, sampled,<br/>event open|closed"]
  u2["<b>status</b> · QoS 1 · retained + LWT<br/>Status: online, sw, config_rev,<br/>loop_active, battery_pct"]
  u3["<b>cp</b> · QoS 1 · retained<br/>CPState: mode, out, ev A–E,<br/>event, plug, slac"]
  u4["<b>telemetry/energy</b> · QoS 1<br/>Telemetry: i_a[3], p_total_w, e_wh"]
  u5["<b>cmd/ack</b> · QoS 1<br/>Ack: pair|power → result"]
  u6["<b>trace</b> · QoS 0<br/>Trace: timing (diagnostics)"]
  D1 --> u1 & u2 & u3 & u4 & u5 & u6
  u1 & u2 & u3 & u4 & u5 & u6 --> E1
```

**Server → device**

```mermaid
flowchart LR
  E2["Engine"]
  D2["Device"]
  d1["<b>v2g/down</b> · QoS 0<br/>Down: exi, close,<br/>cl (cache pair)"]
  d2["<b>cl/push</b> · QoS 1<br/>Push: cache pair without a request"]
  d3["<b>ctrl</b> · QoS 1<br/>Ctrl: ttl_renew"]
  d4["<b>config</b> · QoS 1 · retained<br/>Config: ver, rev, timers, modes"]
  d5["<b>cp/set</b> · QoS 1<br/>CPSet: digital|transit,<br/>restart via X1|F"]
  d6["<b>power/set</b> · QoS 1<br/>PowerSet: limit_a, duration_s, ttl_s"]
  E2 --> d1 & d2 & d3 & d4 & d5 & d6
  d1 & d2 & d3 & d4 & d5 & d6 --> D2
```

| Topic `dev/<id>/…` | Direction | QoS | Retained | Message | When | Engine does |
|---|---|---|---|---|---|---|
| `v2g/up` | device → server | 0 | — | `Up` | session setup (~10 requests), end of session, cache misses; a loop sample once per `loop_sample_s` | steps the ISO / DIN state machine, answers on `v2g/down`; a `sampled` request is journalled, not answered |
| `v2g/down` | server → device | 0 | — | `Down` | answer to every request | — |
| `trace` | device → server | 0 | — | `Trace` | each delivered frame, only with `trace` on | debug journal |
| `cl/push` | server → device | 1 | — | `Push` | limit change during the loop; device reconnect | — |
| `ctrl` | server → device | 1 | — | `Ctrl` (`ttl_renew`) | every `DEVICE_TTL_RENEW_S` (300 s), by the leader | — |
| `config` | server → device | 1 | yes | `Config` | at connect and on change | — |
| `status` | device → server | 1 | yes | `Status`, LWT `{"online":false}` | at connect (`event: connect`), every `heartbeat_s` (60 s) | presence, `device.online/offline`; at connect sends config and the pair again; rule `supply` |
| `cp` | device → server | 1 | yes | `CPState` | every CP change | plug-in / unplug, basic charging, confirms a basic limit |
| `cp/set` | server → device | 1 | — | `CPSet` | mode change, final SoC read (DIN), supply window | — |
| `power/set` | server → device | 1 | — | `PowerSet` | operator limit in basic mode | — |
| `telemetry/energy` | device → server | 1 | — | `Telemetry` | every `telemetry_s` (60 s) while current flows, at once on ≥ 1 A change | `power.changed` in the feed; `telemetry` table |
| `cmd/ack` | device → server | 1 | — | `Ack` | pair or `power/set` taken over | `limit.applied` in the feed (first ack per `command_id` counts) |

Engine instances subscribe through the shared subscription
`$share/engine/dev/+/<topic>`; within one instance a device's messages are
handled in order, across instances they are not (ADR-001). Retained `cp` and
`status` replayed after a (re)subscription are not journalled twice.

## 3. Scenarios

### 3.1. Plug-in and digital session (ISO 15118-2 AC EIM)

```mermaid
sequenceDiagram
  autonumber
  participant EV
  participant D as Device
  participant B as Broker
  participant E as Engine

  EV->>D: CP state B (plugged in)
  D->>B: cp {mode: digital, out: mask, duty: 5, ev: B, event: plugged, plug}
  B->>E: cp
  Note over E: plug-in → session.started
  EV->>D: SLAC (ISO 15118-3), SDP (UDP 15118)
  D-->>EV: SDP response: TLS port if signing available, else TCP
  EV->>D: TCP / TLS 1.2 connect
  D->>B: v2g/up {event: open, conn, tls}
  loop SessionSetup … PowerDelivery(Start), first ChargingStatus
    EV->>D: V2GTP request (EXI)
    D->>B: v2g/up {conn, seq, pt: 0x8001, exi}
    B->>E: v2g/up
    E->>B: v2g/down {seq, exi[, cl]}
    B->>D: v2g/down
    D->>EV: V2GTP response
  end
  Note over E,D: the response to the first ChargingStatusReq carries cl = cache pair (req, res, term, ttl_s)
  D->>B: cmd/ack {kind: pair, ver, result: installed}
```

### 3.2. Charging Loop from the cache, limit change, TTL

```mermaid
sequenceDiagram
  autonumber
  participant EV
  participant D as Device
  participant E as Engine (via broker)
  participant O as Operator (/v1)

  loop every ~1 s
    EV->>D: ChargingStatusReq
    D-->>EV: ChargingStatusRes from the cache (byte match)
  end
  D->>E: v2g/up {sampled: true} at most once per loop_sample_s
  E->>D: ctrl {cmd: ttl_renew, ttl_s} every 300 s
  O->>E: POST /v1/devices/{id}/limit
  E->>D: cl/push {cl: ver+1, command_id}
  D->>E: cmd/ack {kind: pair, command_id, result: installed}
  Note over E: limit.applied in the feed
  EV->>D: ChargingStatusReq
  D-->>EV: response with the new limit
```

### 3.3. Basic mode (no HLC): limit through the PWM mask

```mermaid
sequenceDiagram
  autonumber
  participant EVSE as Station
  participant D as Device
  participant E as Engine
  participant EV

  EVSE->>D: PWM 53.3 % (32 A)
  Note over D,EV: no V2G connection within hlc_wait_s → basic charging
  D->>E: cp {mode: basic, out: transit, ev: C, station: {duty: 53.3}}
  E->>D: power/set {limit_a: 10, duration_s, ttl_s, command_id}
  D->>EV: CP mask 16.7 % (10 A) = AND with station PWM
  D->>E: cmd/ack {kind: power, result: applied}
  D->>E: cp {out: mask, duty: 16.7, limit_a: 10}
  D->>E: telemetry/energy {i_a, p_total_w, e_wh}
```

### 3.4. Server unreachable

```mermaid
sequenceDiagram
  autonumber
  participant EV
  participant D as Device
  participant E as Engine

  Note over D,E: MQTT link lost, broker publishes LWT status {online: false}
  loop until ttl_s expires
    EV->>D: ChargingStatusReq
    D-->>EV: answer from the cache
  end
  Note over D: TTL expired without ttl_renew
  D-->>EV: term[0]: ChargingStatusRes with StopCharging
  EV->>D: PowerDeliveryReq(Stop) / SessionStopReq
  D-->>EV: term[1], term[2] (within term_backup_s)
  Note over D,EV: then transit: basic charging on the station PWM
  Note over D,E: on reconnect: status {event: connect} → config + cl/push
```

A cache miss while offline gets no answer; the EV ends by its own timeout.
Charging never stops because of the server: the worst case is transit of the
station PWM.

## 4. Enumerations

| Field | Values | Meaning |
|---|---|---|
| `Up.event` | `open`, `closed` | EV TCP/TLS connection opened / closed (`reason` with `closed`) |
| `Ctrl.cmd` | `ttl_renew` | extend the pair and limits by `ttl_s` |
| `CPState.mode` | `digital`, `transit`, `basic`, `X1`, `F`, `E` | requested CP mode |
| `CPState.out` | `mask`, `transit`, `X1`, `F`, `E` | applied towards the EV |
| `CPState.ev` | `A`–`E` | EV state, IEC 61851-1 |
| `CPState.event` | `plugged`, `unplugged` | plug-in boundaries (`plug`, `plugged_ms`) |
| `CPState.slac` | `UNMATCHED`, `MATCHING`, `MATCHED` | device's SLAC state |
| `CPSet.mode` | `digital`, `transit` | 5 % to the EV, or station PWM through |
| `CPSet.cmd` / `via` | `restart` / `X1`, `F` | CP restart pulse for `hold_s` |
| `Status.event` | `connect` | first status after (re)connecting |
| `Ack.kind` | `pair`, `power` | what is confirmed |
| `Ack.result` | pair: `installed`, `stale`, `rejected`; power: `applied`, `held`, `cleared`, `stale` | see the contract |
| `Up.pt` | `0x8001` | V2GTP payload type EXI |

## 5. Ports and transports

| Link | Transport | Port | Security |
|---|---|---|---|
| Station ↔ device ↔ EV | CP line, PWM 1 kHz | — | — |
| EV ↔ device | HomePlug GreenPHY (PLC), SLAC | — | — |
| EV ↔ device | SDP | UDP 15118 | — |
| EV ↔ device | V2GTP over TCP or TLS | TCP, port announced in SDP | TLS 1.2 `ECDHE-ECDSA-AES128-SHA256`, SECC key off-device (remote signing) |
| Device ↔ broker | MQTT 3.1.1 over TLS | TCP 8883 (`PUBLIC_TCP_PORT`) | TLS 1.2, client certificate under the deployment's root; ACL `dev/<id>/#` |
| Engine ↔ broker | MQTT 3.1.1 | TCP 1883, compose network only | account `server` |

## 6. Timers and defaults

| Setting | Default | Range | Where |
|---|---|---|---|
| `loop_sample_s` | 30 s | 0–600 | `Config`, `DEVICE_LOOP_SAMPLE_S` |
| `ttl_s` | 1 200 s | 60–3 600 | `Config`, `DEVICE_TTL_S` |
| TTL renewal | 300 s | TTL ≥ 2 × renewal | server, `DEVICE_TTL_RENEW_S` |
| `heartbeat_s` | 60 s | 10–600 | `Config`, `DEVICE_HEARTBEAT_S` |
| `term_backup_s` | 5 s | 2–30 | `Config` |
| `hlc_wait_s` | 30 s | 0–120 | `Config` |
| `telemetry_s` | 60 s | 1–600 | `Config`, `DEVICE_TELEMETRY_S` |
| ISO 15118-2 response budget | 1.5 s (EV timeout 2 s) | — | metric `engine_v2g_server_seconds` |

Per-device values: `PUT /v1/devices/{id}/config`; field list:
`GET /v1/config/fields`.
